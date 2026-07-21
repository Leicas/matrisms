package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/Leicas/matrisms/pkg/common"
	"github.com/Leicas/matrisms/pkg/voipms"
)

// handleMatrixMessageOutbound sends a Matrix message out as SMS or MMS.
//
// Routing: media → sendMMS (with the caption as text); text ≤160 → sendSMS;
// text >160 → split into successive sendSMS calls. The returned MessageID is
// the VoIP.ms id of the (first) sent message, so the poller's echo of our own
// send dedupes against it inside bridgev2.
func (c *SMSClient) handleMatrixMessageOutbound(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if msg.Content == nil {
		return nil, errors.New("no message content")
	}
	if msg.Content.RelatesTo != nil && msg.Content.RelatesTo.Type == event.RelReplace {
		return nil, errors.New("SMS cannot be edited")
	}

	did, peer, ok := common.ParsePortalID(msg.Portal.ID)
	if !ok {
		return nil, fmt.Errorf("cannot send from unrecognized portal %q", msg.Portal.ID)
	}

	body := strings.TrimSpace(msg.Content.Body)

	var voipmsID string
	var isMMS bool
	var err error

	if msg.Content.URL != "" || msg.Content.File != nil {
		// Media message: download from Matrix, send as MMS.
		var media voipms.MediaUpload
		media, err = c.downloadMatrixMedia(ctx, msg.Content)
		if err != nil {
			return nil, fmt.Errorf("failed to download Matrix media: %w", err)
		}
		caption := body
		// For media messages without a real caption, the body is just the
		// filename — don't send that as MMS text.
		if caption == msg.Content.FileName || (msg.Content.FileName == "" && caption == msg.Content.Body && msg.Content.MsgType != event.MsgText && caption != "" && !strings.Contains(caption, " ") && strings.Contains(caption, ".")) {
			caption = ""
		}
		if len(caption) > voipms.MMSMaxTextLen {
			caption = caption[:voipms.MMSMaxTextLen]
		}
		isMMS = true
		voipmsID, err = c.API.SendMMS(ctx, did, peer, caption, []voipms.MediaUpload{media})
	} else {
		if body == "" {
			return nil, errors.New("empty message")
		}
		parts := voipms.SplitSMS(body)
		for i, part := range parts {
			var id string
			id, err = c.API.SendSMS(ctx, did, peer, part)
			if err != nil {
				if i > 0 {
					err = fmt.Errorf("message split into %d SMS; part %d failed: %w", len(parts), i+1, err)
				}
				break
			}
			if i == 0 {
				voipmsID = id
			} else {
				// Only the first part gets a bridgev2 message row (the
				// response can carry a single ID); suppress the poll echo of
				// the remaining parts explicitly or they'd be re-bridged as
				// duplicate incoming messages.
				c.markSentEcho(common.MessageIDFor(false, id))
			}
		}
	}
	if err != nil {
		c.UserLogin.Log.Error().Err(err).Str("peer", peer).Msg("Failed to send message via VoIP.ms")
		return nil, humanizeSendError(err)
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        common.MessageIDFor(isMMS, voipmsID),
			SenderID:  common.PhoneToGhostID(did),
			Timestamp: time.Now(),
		},
	}, nil
}

// downloadMatrixMedia pulls the Matrix media payload (encrypted or plain)
// and validates it against MMS constraints.
func (c *SMSClient) downloadMatrixMedia(ctx context.Context, content *event.MessageEventContent) (voipms.MediaUpload, error) {
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.Bot == nil {
		return voipms.MediaUpload{}, errors.New("bridge bot intent not available")
	}
	data, err := c.UserLogin.Bridge.Bot.DownloadMedia(ctx, content.URL, content.File)
	if err != nil {
		return voipms.MediaUpload{}, err
	}
	if len(data) > voipms.MMSMaxMediaBytes {
		return voipms.MediaUpload{}, fmt.Errorf("attachment is %d bytes; VoIP.ms MMS caps media at %d bytes", len(data), voipms.MMSMaxMediaBytes)
	}
	mime := ""
	if content.Info != nil {
		mime = content.Info.MimeType
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return voipms.MediaUpload{Data: data, Mime: mime}, nil
}

// humanizeSendError maps well-known VoIP.ms error statuses to messages that
// make sense to the person typing in the Matrix room.
func humanizeSendError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "limit_reached"):
		return errors.New("VoIP.ms daily API message limit reached (default 100/day — contact VoIP.ms support to raise it)")
	case strings.Contains(msg, "invalid_dst"):
		return errors.New("VoIP.ms rejected the destination number (SMS works only to 10-digit US/Canada numbers)")
	case strings.Contains(msg, "sms_toolong"):
		return errors.New("message too long for a single SMS")
	case strings.Contains(msg, "non_sufficient_funds"):
		return errors.New("VoIP.ms account balance is too low to send")
	case strings.Contains(msg, "invalid_credentials"), strings.Contains(msg, "ip_not_enabled"), strings.Contains(msg, "api_not_enabled"):
		return fmt.Errorf("VoIP.ms API auth problem (%w) — check API enablement and IP whitelist", err)
	default:
		return err
	}
}
