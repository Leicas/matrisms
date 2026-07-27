package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/Leicas/matrisms/pkg/common"
	"github.com/Leicas/matrisms/pkg/voipms"
)

// handleRemoteMessage is the poller's OnMessage callback: build the remote
// event, ensure the portal/room exists, queue for delivery. bridgev2 dedupes
// on the network message ID, so day-granular poll overlap (and our own
// outbound echoes, which were stored under the same ID at send time) are
// dropped by the framework.
func (c *SMSClient) handleRemoteMessage(ctx context.Context, msg *voipms.Message) error {
	if !msg.Inbound && c.isSentEcho(common.MessageIDFor(msg.IsMMS, msg.ID)) {
		return nil // echo of a message this bridge sent without a DB row
	}
	portalKey := networkid.PortalKey{
		ID:       common.PortalIDFor(msg.DID, msg.Contact),
		Receiver: c.UserLogin.ID,
	}

	// VoIP.ms sometimes reassembles long multipart messages in the wrong
	// segment order; repair before anything else looks at the body.
	if !msg.IsMMS && c.Main.Config.VoIPms.EffectiveUnscrambleSegments() {
		if fixed, ok := voipms.RepairScrambledBody(msg.Body); ok {
			c.UserLogin.Log.Info().Str("message_id", msg.ID).Msg("Repaired scrambled multipart SMS segment order")
			msg.Body = fixed
		}
	}

	// Reaction fallbacks ("Reacted 😂 to \"…\"", « A réagi 😂 à … ») become
	// real Matrix reactions when the quoted message can be found; otherwise
	// they fall through and bridge as plain text.
	if !msg.IsMMS && c.Main.Config.VoIPms.EffectiveConvertReactions() {
		if fb, ok := common.ParseReactionFallback(msg.Body); ok {
			if target := c.findReactionTarget(ctx, portalKey, fb); target != "" {
				if !c.UserLogin.QueueRemoteEvent(&SMSReactionEvent{
					msg: msg, client: c, portalKey: portalKey,
					target: target, fallback: fb,
				}).Success {
					return fmt.Errorf("queue remote reaction failed for message %s", msg.ID)
				}
				return nil
			}
			c.UserLogin.Log.Debug().Str("message_id", msg.ID).Msg("Reaction fallback text matched no recent message; bridging as text")
		}
	}

	evt := &SMSMatrixEvent{msg: msg, client: c, portalKey: portalKey}

	portal, err := c.UserLogin.Bridge.GetExistingPortalByKey(ctx, portalKey)
	if err != nil {
		return fmt.Errorf("portal lookup: %w", err)
	}
	if portal == nil {
		portal, err = c.UserLogin.Bridge.GetPortalByKey(ctx, portalKey)
		if err != nil {
			return fmt.Errorf("create portal: %w", err)
		}
	}
	if portal.MXID == "" {
		if err := portal.CreateMatrixRoom(ctx, c.UserLogin, nil); err != nil {
			return fmt.Errorf("create matrix room: %w", err)
		}
	}

	if !c.UserLogin.QueueRemoteEvent(evt).Success {
		return fmt.Errorf("queue remote event failed for message %s", msg.ID)
	}
	return nil
}

// findReactionTarget scans recent messages in the portal (newest first) for
// the one the fallback text quotes. Returns "" when nothing matches.
func (c *SMSClient) findReactionTarget(ctx context.Context, portalKey networkid.PortalKey, fb *common.ReactionFallback) networkid.MessageID {
	recent, err := c.UserLogin.Bridge.DB.Message.GetLastNInPortal(ctx, portalKey, 50)
	if err != nil {
		c.UserLogin.Log.Warn().Err(err).Msg("Failed to load recent messages for reaction target lookup")
		return ""
	}
	for _, dbMsg := range recent {
		meta, ok := dbMsg.Metadata.(*SMSMessageMetadata)
		if !ok || meta.Body == "" {
			continue
		}
		if fb.MatchesTarget(meta.Body) {
			return dbMsg.ID
		}
	}
	return ""
}

// SMSReactionEvent is a reaction-fallback SMS converted into a Matrix
// reaction (or reaction removal) on the quoted message.
type SMSReactionEvent struct {
	msg       *voipms.Message
	client    *SMSClient
	portalKey networkid.PortalKey
	target    networkid.MessageID
	fallback  *common.ReactionFallback
}

var (
	_ bridgev2.RemoteReaction           = (*SMSReactionEvent)(nil)
	_ bridgev2.RemoteReactionRemove     = (*SMSReactionEvent)(nil)
	_ bridgev2.RemoteEventWithTimestamp = (*SMSReactionEvent)(nil)
)

func (e *SMSReactionEvent) GetType() bridgev2.RemoteEventType {
	if e.fallback.Remove {
		return bridgev2.RemoteEventReactionRemove
	}
	return bridgev2.RemoteEventReaction
}

func (e *SMSReactionEvent) GetPortalKey() networkid.PortalKey { return e.portalKey }
func (e *SMSReactionEvent) GetTimestamp() time.Time           { return e.msg.Date }

func (e *SMSReactionEvent) GetSender() bridgev2.EventSender {
	if !e.msg.Inbound {
		return bridgev2.EventSender{Sender: common.PhoneToGhostID(e.msg.DID), IsFromMe: true}
	}
	return bridgev2.EventSender{Sender: common.PhoneToGhostID(e.msg.Contact)}
}

func (e *SMSReactionEvent) GetTargetMessage() networkid.MessageID { return e.target }

// GetReactionEmoji returns an empty EmojiID: SMS senders effectively have one
// reaction per message, so a new reaction replaces the previous one.
func (e *SMSReactionEvent) GetReactionEmoji() (string, networkid.EmojiID) {
	return e.fallback.Emoji, ""
}

func (e *SMSReactionEvent) GetRemovedEmojiID() networkid.EmojiID { return "" }

func (e *SMSReactionEvent) AddLogContext(c zerolog.Context) zerolog.Context {
	return c.
		Str("voipms_message_id", e.msg.ID).
		Str("reaction_target", string(e.target)).
		Str("emoji", e.fallback.Emoji).
		Bool("remove", e.fallback.Remove)
}

// SMSMatrixEvent implements bridgev2.RemoteMessage for one inbound (or
// echoed outbound) VoIP.ms message.
type SMSMatrixEvent struct {
	msg       *voipms.Message
	client    *SMSClient
	portalKey networkid.PortalKey
}

var _ bridgev2.RemoteMessage = (*SMSMatrixEvent)(nil)

func (e *SMSMatrixEvent) GetID() networkid.MessageID {
	return common.MessageIDFor(e.msg.IsMMS, e.msg.ID)
}

func (e *SMSMatrixEvent) GetTimestamp() time.Time {
	return e.msg.Date
}

func (e *SMSMatrixEvent) GetSender() bridgev2.EventSender {
	if !e.msg.Inbound {
		// Message sent from our DID (bridge echo or sent from the VoIP.ms
		// portal / another client): attribute to us. The Sender ghost ID
		// matches what client_send.go stores so DB rows stay consistent.
		return bridgev2.EventSender{Sender: common.PhoneToGhostID(e.msg.DID), IsFromMe: true}
	}
	return bridgev2.EventSender{Sender: common.PhoneToGhostID(e.msg.Contact)}
}

func (e *SMSMatrixEvent) GetPortalKey() networkid.PortalKey {
	return e.portalKey
}

func (e *SMSMatrixEvent) GetType() bridgev2.RemoteEventType {
	return bridgev2.RemoteEventMessage
}

func (e *SMSMatrixEvent) AddLogContext(c zerolog.Context) zerolog.Context {
	return c.
		Str("voipms_message_id", e.msg.ID).
		Bool("is_mms", e.msg.IsMMS).
		Bool("inbound", e.msg.Inbound)
}

// ConvertMessage renders the message into Matrix parts: one text part (if
// there's a body) plus one media part per MMS attachment. Media is fetched
// from VoIP.ms and re-uploaded to the Matrix media repo.
func (e *SMSMatrixEvent) ConvertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI) (*bridgev2.ConvertedMessage, error) {
	var parts []*bridgev2.ConvertedMessagePart

	if e.msg.Body != "" {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			ID:   networkid.PartID("text"),
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    e.msg.Body,
			},
			// Body is kept in the DB row so later reaction fallbacks can be
			// matched back to this message.
			DBMetadata: &SMSMessageMetadata{Body: e.msg.Body},
		})
	}

	media := e.msg.Media
	if e.msg.IsMMS && len(media) == 0 {
		// getMMS frequently returns empty col_media fields even for image
		// MMS; getMediaMMS is the reliable way to discover attachments.
		fetched, err := e.client.API.GetMMSMedia(ctx, e.msg.ID)
		if err != nil {
			e.client.UserLogin.Log.Warn().Err(err).Str("mms_id", e.msg.ID).Msg("getMediaMMS failed")
		} else {
			media = fetched
		}
	}

	maxBytes := e.client.Main.Config.VoIPms.EffectiveMaxUploadBytes()
	for i, mediaURL := range media {
		part, err := e.convertMediaToMatrix(ctx, intent, mediaURL, i, maxBytes)
		if err != nil {
			e.client.UserLogin.Log.Warn().Err(err).Str("media_url", mediaURL).Msg("Failed to bridge MMS media")
			parts = append(parts, &bridgev2.ConvertedMessagePart{
				ID:   networkid.PartID(fmt.Sprintf("media-error-%d", i)),
				Type: event.EventMessage,
				Content: &event.MessageEventContent{
					MsgType: event.MsgNotice,
					Body:    fmt.Sprintf("⚠️ An MMS attachment could not be bridged (%s)", err),
				},
			})
			continue
		}
		parts = append(parts, part)
	}

	if len(parts) == 0 {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			ID:   networkid.PartID("empty"),
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    "(empty message)",
			},
		})
	}

	return &bridgev2.ConvertedMessage{Parts: parts}, nil
}

func (e *SMSMatrixEvent) convertMediaToMatrix(ctx context.Context, intent bridgev2.MatrixAPI, mediaURL string, index, maxBytes int) (*bridgev2.ConvertedMessagePart, error) {
	data, mime, err := e.client.API.DownloadMedia(ctx, mediaURL, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}

	filename := fmt.Sprintf("mms-%s-%d%s", e.msg.ID, index+1, extensionForMime(mime))
	uploadURL, _, err := intent.UploadMedia(ctx, "", data, filename, mime)
	if err != nil {
		return nil, fmt.Errorf("upload to Matrix: %w", err)
	}

	return &bridgev2.ConvertedMessagePart{
		ID:   networkid.PartID(fmt.Sprintf("media-%d", index)),
		Type: event.EventMessage,
		Content: &event.MessageEventContent{
			MsgType: msgTypeForMime(mime),
			Body:    filename,
			URL:     uploadURL,
			Info: &event.FileInfo{
				MimeType: mime,
				Size:     len(data),
			},
		},
	}, nil
}

func msgTypeForMime(mime string) event.MessageType {
	switch {
	case len(mime) >= 6 && mime[:6] == "image/":
		return event.MsgImage
	case len(mime) >= 6 && mime[:6] == "video/":
		return event.MsgVideo
	case len(mime) >= 6 && mime[:6] == "audio/":
		return event.MsgAudio
	default:
		return event.MsgFile
	}
}

func extensionForMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/3gpp":
		return ".3gp"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/midi":
		return ".mid"
	default:
		return ""
	}
}
