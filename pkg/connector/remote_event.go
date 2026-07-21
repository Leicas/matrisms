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
