package connector

import (
	"context"
	"fmt"
	"sync/atomic"

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

	ctx         context.Context
	cancel      context.CancelFunc
	isConnected atomic.Bool
}

var _ bridgev2.NetworkAPI = (*SMSClient)(nil)

// SMSLoginMetadata is the (non-secret) metadata stored on the bridgev2
// UserLogin row. Credentials live encrypted in the connector's own table.
type SMSLoginMetadata struct {
	APIUsername string `json:"api_username"`
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
// after LoadUserLogin.
func (c *SMSClient) Connect(ctx context.Context) {
	c.stateCoordinator.ReportSimpleEvent("poller", "connection_started", false, "", nil)

	// Validate credentials once before declaring the connection healthy.
	if _, err := c.API.VerifyCredentials(c.ctx); err != nil {
		c.UserLogin.Log.Error().Err(err).Msg("VoIP.ms credential check failed")
		errCode := coordinator.SMSConnectionFailed
		if voipms.IsAuthError(err) {
			errCode = SMSBadCredentials
		}
		c.stateCoordinator.ReportSimpleEvent("poller", "auth_failure", false, errCode, map[string]any{"go_error": err.Error()})
		return
	}

	c.isConnected.Store(true)
	c.stateCoordinator.ReportSimpleEvent("poller", "connection_established", true, "", nil)
	c.stateCoordinator.ReportSimpleEvent("poller", "idle_started", true, "", nil)

	go func() {
		err := c.Poller.Run(c.ctx)
		if err != nil && c.ctx.Err() == nil {
			c.UserLogin.Log.Error().Err(err).Msg("Poller stopped unexpectedly")
			c.stateCoordinator.ReportSimpleEvent("poller", "connection_lost", false, coordinator.SMSPollFailed, map[string]any{"go_error": err.Error()})
		}
	}()

	c.UserLogin.Log.Info().Strs("dids", c.DIDs).Msg("VoIP.ms SMS client connected; polling started")
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
	for _, did := range c.DIDs {
		if userID == common.PhoneToGhostID(did) {
			return true
		}
	}
	return false
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
				MaxSize:   voipms.MMSMaxMediaBytes,
				Caption:   event.CapLevelFullySupported,
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
		Reply:           event.CapLevelDropped, // sent as a plain message
		Edit:            event.CapLevelRejected,
		Delete:          event.CapLevelRejected,
		Reaction:        event.CapLevelRejected,
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
