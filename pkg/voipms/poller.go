package voipms

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/rs/zerolog"
)

// Cursor is the persisted poll position for one (account, DID) pair. The
// API's date filters are day-granular, so each poll re-fetches from the day
// of the last seen message and relies on message-id dedup downstream
// (bridgev2 ignores remote messages whose network ID already exists).
type Cursor struct {
	// LastDate is the timestamp (UTC) of the newest message ever handed to
	// OnMessage.
	LastDate time.Time `json:"last_date"`
}

func (c Cursor) Encode() string {
	data, err := json.Marshal(c)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func DecodeCursor(raw string) Cursor {
	var c Cursor
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	return c
}

// Poller polls one VoIP.ms account for new messages across its monitored
// DIDs. Mirrors the REST-poller shape proven in matrimail's Gmail poller:
// cursor bootstrap, tick loop, per-tick fetch, cursor persist.
type Poller struct {
	Client *Client
	// DIDs to poll, normalized digits.
	DIDs []string
	// PollInterval between sweeps. Also see TriggerPoll for webhook nudges.
	PollInterval time.Duration
	// Backfill is how far back the very first poll (no stored cursor) looks.
	Backfill time.Duration
	// CursorLoad / CursorSave persist the per-DID cursor.
	CursorLoad func(ctx context.Context, did string) (string, error)
	CursorSave func(ctx context.Context, did, cursor string) error
	// OnMessage is called for every fetched message, oldest first. It must
	// be idempotent — date filters are day-granular so overlap re-delivery
	// is normal. Returning an error stops cursor advancement for that DID.
	OnMessage func(ctx context.Context, msg *Message) error
	// OnError is notified of poll-cycle errors (for bridge state reporting).
	// Optional.
	OnError func(err error)
	Log     *zerolog.Logger

	trigger chan struct{}
}

// TriggerPoll requests an immediate sweep (e.g. when a webhook callback
// fires). Non-blocking; coalesces with any pending trigger.
func (p *Poller) TriggerPoll() {
	if p.trigger == nil {
		return
	}
	select {
	case p.trigger <- struct{}{}:
	default:
	}
}

// Run polls until ctx is cancelled. Returns ctx.Err().
func (p *Poller) Run(ctx context.Context) error {
	p.trigger = make(chan struct{}, 1)
	interval := p.PollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			p.pollOnce(ctx)
		case <-p.trigger:
			p.pollOnce(ctx)
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	for _, did := range p.DIDs {
		if ctx.Err() != nil {
			return
		}
		if err := p.pollDID(ctx, did); err != nil {
			if p.Log != nil {
				p.Log.Warn().Err(err).Str("did", did).Msg("Poll cycle failed")
			}
			if p.OnError != nil {
				p.OnError(err)
			}
		}
	}
}

func (p *Poller) pollDID(ctx context.Context, did string) error {
	raw, err := p.CursorLoad(ctx, did)
	if err != nil {
		return err
	}
	cursor := DecodeCursor(raw)

	now := time.Now().UTC()
	from := cursor.LastDate
	if from.IsZero() {
		backfill := p.Backfill
		if backfill < 0 {
			backfill = 0
		}
		from = now.Add(-backfill)
	} else {
		// Date filters are day-granular; step back half a day to be safe
		// around midnight boundaries in the API's timezone.
		from = from.Add(-12 * time.Hour)
	}
	// The API rejects ranges over 92 days; clamp (older messages are
	// intentionally skipped — a bridge poll, not an archive import).
	if now.Sub(from) > 90*24*time.Hour {
		from = now.Add(-90 * 24 * time.Hour)
	}

	msgs, err := p.Client.GetMessages(ctx, did, from, now)
	if err != nil {
		return err
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].Date.Before(msgs[j].Date) })

	newest := cursor.LastDate
	for i := range msgs {
		msg := &msgs[i]
		if err := p.OnMessage(ctx, msg); err != nil {
			// Persist progress up to the failure and retry the rest next tick.
			if p.Log != nil {
				p.Log.Warn().Err(err).Str("message_id", msg.ID).Msg("OnMessage failed; will retry from cursor")
			}
			break
		}
		if msg.Date.After(newest) {
			newest = msg.Date
		}
	}
	if newest.After(cursor.LastDate) {
		cursor.LastDate = newest
		if err := p.CursorSave(ctx, did, cursor.Encode()); err != nil {
			return err
		}
	}
	return nil
}
