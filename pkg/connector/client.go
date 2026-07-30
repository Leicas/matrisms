package connector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/Leicas/matrisms/pkg/common"
	"github.com/Leicas/matrisms/pkg/coordinator"
	"github.com/Leicas/matrisms/pkg/voipms"
)

// SMSClient is the per-login bridgev2.NetworkAPI implementation. One client
// = one VoIP.ms account (api username), possibly polling several DIDs.
type SMSClient struct {
	Main      *SMSConnector
	UserLogin *bridgev2.UserLogin

	APIUsername string
	DIDs        []string

	API    *voipms.Client
	Poller *voipms.Poller

	stateCoordinator *coordinator.StateCoordinator

	// phonebook caches VoIP.ms phonebook names by normalized number.
	phonebookMu sync.Mutex
	phonebook   map[string]string
	phonebookAt time.Time

	// sentEchoIDs holds network message IDs of messages this bridge just sent
	// whose poll echo must be skipped because bridgev2 has no DB row for them
	// (parts 2..n of a split SMS, `!matrisms text` sends). Values are insert
	// times for pruning.
	sentEchoMu  sync.Mutex
	sentEchoIDs map[networkid.MessageID]time.Time

	ctx         context.Context
	cancel      context.CancelFunc
	isConnected atomic.Bool
}

// markSentEcho records a just-sent message ID so its poll echo is dropped.
func (c *SMSClient) markSentEcho(id networkid.MessageID) {
	c.sentEchoMu.Lock()
	defer c.sentEchoMu.Unlock()
	if c.sentEchoIDs == nil {
		c.sentEchoIDs = make(map[networkid.MessageID]time.Time)
	}
	now := time.Now()
	for k, t := range c.sentEchoIDs {
		if now.Sub(t) > 24*time.Hour {
			delete(c.sentEchoIDs, k)
		}
	}
	c.sentEchoIDs[id] = now
}

// isSentEcho reports whether a message ID was recorded by markSentEcho.
// The entry is kept (not popped): day-granular polling re-delivers the same
// message on every sweep until the cursor day rolls over.
func (c *SMSClient) isSentEcho(id networkid.MessageID) bool {
	c.sentEchoMu.Lock()
	defer c.sentEchoMu.Unlock()
	_, ok := c.sentEchoIDs[id]
	return ok
}

// ContactName returns the phonebook name for a number, or "" when unknown or
// phonebook naming is disabled. Results are cached per the configured TTL.
func (c *SMSClient) ContactName(ctx context.Context, number string) string {
	if !c.Main.Config.VoIPms.EffectivePhonebookNames() {
		return ""
	}
	number = common.NormalizePhone(number)
	c.phonebookMu.Lock()
	defer c.phonebookMu.Unlock()
	if time.Since(c.phonebookAt) > c.Main.Config.VoIPms.EffectivePhonebookRefresh() {
		entries, err := c.API.GetPhonebook(ctx)
		if err != nil {
			c.UserLogin.Log.Warn().Err(err).Msg("Failed to refresh phonebook; keeping cached names")
			// Back off for a minute so a broken phonebook API doesn't add a
			// failing call to every ghost/room info fetch.
			c.phonebookAt = time.Now().Add(time.Minute - c.Main.Config.VoIPms.EffectivePhonebookRefresh())
		} else {
			c.phonebook = make(map[string]string, len(entries))
			for _, e := range entries {
				if len(e.Number) >= 10 && e.Name != "" {
					c.phonebook[e.Number] = e.Name
				}
			}
			c.phonebookAt = time.Now()
		}
	}
	return c.phonebook[number]
}

// InvalidatePhonebook drops the phonebook cache so the next lookup re-fetches.
func (c *SMSClient) InvalidatePhonebook() {
	c.phonebookMu.Lock()
	defer c.phonebookMu.Unlock()
	c.phonebookAt = time.Time{}
}

// OwnsDID reports whether a normalized number is one of this login's DIDs.
func (c *SMSClient) OwnsDID(number string) bool {
	for _, did := range c.DIDs {
		if did == number {
			return true
		}
	}
	return false
}

var _ bridgev2.NetworkAPI = (*SMSClient)(nil)

// SMSLoginMetadata is the (non-secret) metadata stored on the bridgev2
// UserLogin row. Credentials live encrypted in the connector's own table.
type SMSLoginMetadata struct {
	APIUsername string `json:"api_username"`
}

// SMSMessageMetadata is stored on every bridged message row. Body lets
// reaction-fallback texts ("Reacted 😂 to \"…\"") be resolved back to the
// message they quote.
type SMSMessageMetadata struct {
	Body string `json:"body,omitempty"`
}

// LoadUserLogin creates the SMSClient for a stored login. Called by the
// framework at startup for every saved login and right after a new login
// completes.
func (sc *SMSConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta, ok := login.Metadata.(*SMSLoginMetadata)
	if !ok || meta.APIUsername == "" {
		return fmt.Errorf("user login %s has no API username metadata", login.ID)
	}

	account, err := sc.DB.GetAccount(ctx, login.UserMXID.String(), meta.APIUsername)
	if err != nil {
		return fmt.Errorf("failed to load account credentials: %w", err)
	}
	if account == nil {
		return fmt.Errorf("no stored VoIP.ms account for %s — run login again", meta.APIUsername)
	}

	clientCtx, cancel := context.WithCancel(context.Background())
	apiLog := login.Log.With().Str("component", "voipms").Logger()
	client := &SMSClient{
		Main:        sc,
		UserLogin:   login,
		APIUsername: account.APIUsername,
		DIDs:        account.DIDs,
		API:         voipms.NewClient(sc.Config.VoIPms.EffectiveBaseURL(), account.APIUsername, account.APIPassword, &apiLog),
		ctx:         clientCtx,
		cancel:      cancel,
	}
	client.stateCoordinator = coordinator.NewStateCoordinator(login, &apiLog)

	pollLog := login.Log.With().Str("component", "poller").Logger()
	client.Poller = &voipms.Poller{
		Client:       client.API,
		DIDs:         account.DIDs,
		PollInterval: sc.Config.VoIPms.EffectivePollInterval(),
		Backfill:     sc.Config.VoIPms.EffectiveBackfill(),
		CursorLoad: func(ctx context.Context, did string) (string, error) {
			return sc.DB.GetPollCursor(ctx, login.UserMXID.String(), account.APIUsername, did)
		},
		CursorSave: func(ctx context.Context, did, cursor string) error {
			return sc.DB.SetPollCursor(ctx, login.UserMXID.String(), account.APIUsername, did, cursor)
		},
		OnMessage: client.handleRemoteMessage,
		OnError:   client.handlePollError,
		Log:       &pollLog,
	}

	login.Client = client
	sc.registerClient(login.ID, client)
	return nil
}

// Connect starts the poll loop. The framework calls this in a goroutine
// after LoadUserLogin. The connection is supervised: a failed credential check
// or a poll loop that dies is retried with exponential backoff, so a transient
// VoIP.ms outage no longer stops inbound SMS until the next manual restart.
func (c *SMSClient) Connect(ctx context.Context) {
	c.stateCoordinator.ReportSimpleEvent("poller", "connection_started", false, "", nil)
	go c.supervise()
}

// supervise runs connectAndPoll, retrying on failure until the client is
// disconnected, the credentials are definitively rejected, or the configured
// retry budget is exhausted.
func (c *SMSClient) supervise() {
	cfg := c.Main.Config.VoIPms
	backoff := cfg.EffectiveConnectRetryInterval()
	maxBackoff := cfg.EffectiveConnectRetryMaxInterval()
	attempt := 0

	for c.ctx.Err() == nil {
		connected, err := c.connectAndPoll()
		if err == nil {
			// Deliberate disconnect.
			return
		}
		if connected {
			// The connection worked at least once, so this is a fresh failure
			// rather than a continuing one: restart the backoff ramp.
			backoff = cfg.EffectiveConnectRetryInterval()
			attempt = 0
		}
		if voipms.IsAuthError(err) {
			// Bad credentials won't fix themselves; the user must re-login.
			c.UserLogin.Log.Error().Err(err).Msg("VoIP.ms credentials rejected; not retrying")
			return
		}
		if !cfg.ConnectRetryEnabled() {
			c.UserLogin.Log.Error().Err(err).Msg("VoIP.ms connection failed and retries are disabled")
			return
		}
		attempt++
		if cfg.ConnectMaxRetries > 0 && attempt > cfg.ConnectMaxRetries {
			c.UserLogin.Log.Error().Err(err).
				Int("attempts", attempt-1).
				Msg("VoIP.ms connection retry budget exhausted; giving up until restart")
			return
		}
		c.UserLogin.Log.Warn().Err(err).
			Int("attempt", attempt).
			Dur("retry_in", backoff).
			Msg("Retrying VoIP.ms connection after failure")
		if !c.sleepCtx(backoff) {
			return
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// sleepCtx waits for d, returning false if the client was disconnected first.
func (c *SMSClient) sleepCtx(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// connectAndPoll verifies credentials and then runs the poll loop until it
// stops. connected reports whether polling actually started, and err is why it
// stopped — nil meaning the client was disconnected deliberately.
func (c *SMSClient) connectAndPoll() (connected bool, err error) {
	if _, err = c.API.VerifyCredentials(c.ctx); err != nil {
		if c.ctx.Err() != nil {
			return false, nil
		}
		c.UserLogin.Log.Error().Err(err).Msg("VoIP.ms credential check failed")
		errCode := coordinator.SMSConnectionFailed
		if voipms.IsAuthError(err) {
			errCode = SMSBadCredentials
		}
		c.stateCoordinator.ReportSimpleEvent("poller", "auth_failure", false, errCode, map[string]any{"go_error": err.Error()})
		return false, err
	}

	c.isConnected.Store(true)
	c.stateCoordinator.ReportSimpleEvent("poller", "connection_established", true, "", nil)
	c.stateCoordinator.ReportSimpleEvent("poller", "idle_started", true, "", nil)
	c.UserLogin.Log.Info().Strs("dids", c.DIDs).Msg("VoIP.ms SMS client connected; polling started")

	err = c.Poller.Run(c.ctx)
	c.isConnected.Store(false)
	if c.ctx.Err() != nil {
		return true, nil
	}
	if err == nil {
		err = fmt.Errorf("poll loop stopped without an error")
	}
	c.UserLogin.Log.Error().Err(err).Msg("Poller stopped unexpectedly")
	c.stateCoordinator.ReportSimpleEvent("poller", "connection_lost", false, coordinator.SMSPollFailed, map[string]any{"go_error": err.Error()})
	return true, err
}

func (c *SMSClient) handlePollError(err error) {
	if voipms.IsAuthError(err) {
		c.isConnected.Store(false)
		c.stateCoordinator.ReportSimpleEvent("poller", "auth_failure", false, SMSBadCredentials, map[string]any{"go_error": err.Error()})
	}
}

const SMSBadCredentials = status.BridgeStateErrorCode("E-SMS-001")

func (c *SMSClient) Disconnect() {
	c.isConnected.Store(false)
	if c.cancel != nil {
		c.cancel()
	}
	c.Main.unregisterClient(c.UserLogin.ID, c)
	c.UserLogin.Log.Info().Msg("VoIP.ms SMS client disconnected")
}

func (c *SMSClient) IsLoggedIn() bool {
	return c.isConnected.Load()
}

func (c *SMSClient) IsConnected() bool {
	return c.isConnected.Load()
}

func (c *SMSClient) LogoutRemote(ctx context.Context) {
	c.UserLogin.Log.Info().Msg("Logging out VoIP.ms account")
	if err := c.Main.DB.DeleteAccount(ctx, c.UserLogin.UserMXID.String(), c.APIUsername); err != nil {
		c.UserLogin.Log.Error().Err(err).Msg("Failed to delete account from database")
	}
	c.Disconnect()
}

// Stop is called by StoppableNetwork teardown paths.
func (c *SMSClient) Stop(ctx context.Context) {
	c.Disconnect()
}

func (c *SMSClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	// Our own side never appears as a ghost — outbound messages are
	// attributed via IsFromMe. Check against our DIDs anyway for safety.
	number := strings.TrimPrefix(string(userID), "sms:")
	return c.OwnsDID(number)
}

func (c *SMSClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	return c.Main.GetChatInfo(ctx, portal, c.UserLogin, networkid.PortalKey{ID: portal.ID, Receiver: c.UserLogin.ID})
}

func (c *SMSClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	return c.Main.GetUserInfo(ctx, ghost)
}

// GetCapabilities declares what an SMS room supports. The framework's
// Portal.checkMessageContentCaps consults File BEFORE HandleMatrixMessage
// and hard-rejects undeclared media msgtypes, so every MMS-able type must
// appear here.
func (c *SMSClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	mmsMime := map[string]event.CapabilitySupportLevel{
		"image/jpeg": event.CapLevelFullySupported,
		"image/png":  event.CapLevelFullySupported,
		"image/gif":  event.CapLevelFullySupported,
		// Converted to JPEG on send (FitImageToMMS decodes webp).
		"image/webp": event.CapLevelPartialSupport,
	}
	return &event.RoomFeatures{
		ID: "matrisms-voipms-v1",
		Formatting: event.FormattingFeatureMap{
			// SMS is plain text: everything gets flattened to the body.
			event.FmtBold:          event.CapLevelDropped,
			event.FmtItalic:        event.CapLevelDropped,
			event.FmtUnderline:     event.CapLevelDropped,
			event.FmtStrikethrough: event.CapLevelDropped,
			event.FmtInlineCode:    event.CapLevelDropped,
			event.FmtCodeBlock:     event.CapLevelDropped,
			event.FmtBlockquote:    event.CapLevelDropped,
			event.FmtInlineLink:    event.CapLevelDropped,
			event.FmtUnorderedList: event.CapLevelDropped,
			event.FmtOrderedList:   event.CapLevelDropped,
			event.FmtHeaders:       event.CapLevelDropped,
			event.FmtSpoiler:       event.CapLevelDropped,
			event.FmtCustomEmoji:   event.CapLevelDropped,
		},
		File: event.FileFeatureMap{
			event.MsgImage: {
				MimeTypes: mmsMime,
				// Larger images are shrunk to the ~1.3 MB MMS cap on send.
				MaxSize: DefaultMaxUploadBytes,
				Caption: event.CapLevelFullySupported,
			},
			event.MsgVideo: {
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"video/mp4":  event.CapLevelFullySupported,
					"video/3gpp": event.CapLevelFullySupported,
				},
				MaxSize: voipms.MMSMaxMediaBytes,
				Caption: event.CapLevelFullySupported,
			},
			event.MsgAudio: {
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"audio/mpeg": event.CapLevelFullySupported,
					"audio/wav":  event.CapLevelFullySupported,
					"audio/midi": event.CapLevelFullySupported,
				},
				MaxSize: voipms.MMSMaxMediaBytes,
				Caption: event.CapLevelFullySupported,
			},
		},
		Reply:  event.CapLevelDropped, // sent as a plain message
		Edit:   event.CapLevelRejected,
		Delete: event.CapLevelRejected,
		// Sent as a fallback text ('Reacted 😂 to "…"'), the same convention
		// phones use when a reaction can't ride a rich channel.
		Reaction:        event.CapLevelPartialSupport,
		ReactionCount:   1,
		Thread:          event.CapLevelUnsupported,
		LocationMessage: event.CapLevelRejected,
		Poll:            event.CapLevelRejected,
	}
}

// HandleMatrixMessage implements the NetworkAPI interface; the full
// outbound flow lives in client_send.go.
func (c *SMSClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	return c.handleMatrixMessageOutbound(ctx, msg)
}
