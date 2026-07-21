package connector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/Leicas/matrisms/pkg/common"
)

// SMSConnector is the bridgev2 NetworkConnector for VoIP.ms SMS/MMS.
type SMSConnector struct {
	Bridge *bridgev2.Bridge
	Config Config
	DB     *SMSAccountQuery

	// Webhook is the optional instant-delivery HTTP listener (VoIP.ms
	// "SMS URL Callback"). Created in Init, started in Start.
	Webhook *WebhookServer

	// initCancel tears down connector-scoped background work on Stop.
	initCancel context.CancelFunc

	// clients tracks active per-login SMSClients so the webhook listener
	// can route callbacks by DID (bridgev2 has no public "all cached
	// logins" enumerator).
	clientsMu sync.Mutex
	clients   map[networkid.UserLoginID]*SMSClient

	// networkIcon holds the mxc URI (string) of the uploaded bridge logo,
	// set by setupLogo and served through GetName.
	networkIcon atomic.Value
}

func (sc *SMSConnector) registerClient(id networkid.UserLoginID, client *SMSClient) {
	sc.clientsMu.Lock()
	defer sc.clientsMu.Unlock()
	if sc.clients == nil {
		sc.clients = map[networkid.UserLoginID]*SMSClient{}
	}
	sc.clients[id] = client
}

func (sc *SMSConnector) unregisterClient(id networkid.UserLoginID, client *SMSClient) {
	sc.clientsMu.Lock()
	defer sc.clientsMu.Unlock()
	if sc.clients[id] == client {
		delete(sc.clients, id)
	}
}

// activeClients returns a snapshot of the registered per-login clients.
func (sc *SMSConnector) activeClients() []*SMSClient {
	sc.clientsMu.Lock()
	defer sc.clientsMu.Unlock()
	out := make([]*SMSClient, 0, len(sc.clients))
	for _, c := range sc.clients {
		out = append(out, c)
	}
	return out
}

var (
	_ bridgev2.NetworkConnector = (*SMSConnector)(nil)
	_ bridgev2.StoppableNetwork = (*SMSConnector)(nil)
)

func (sc *SMSConnector) GetName() bridgev2.BridgeName {
	icon, _ := sc.networkIcon.Load().(string)
	return bridgev2.BridgeName{
		DisplayName:          "Matrisms",
		NetworkURL:           "https://voip.ms",
		NetworkIcon:          id.ContentURIString(icon),
		NetworkID:            "voipms-sms",
		BeeperBridgeType:     "voipms-sms",
		DefaultPort:          29331,
		DefaultCommandPrefix: "!matrisms",
	}
}

func (sc *SMSConnector) Init(bridge *bridgev2.Bridge) {
	sc.Bridge = bridge

	// NOTE: never replace sc.Config wholesale here — the framework already
	// decoded the user's network block into it. Only fill zero-value defaults
	// (the Effective* accessors handle that at point of use).

	// Allow environment overrides for log verbosity.
	// MATRISMS_LOG_LEVEL: trace|debug|info|warn|error
	if lvl := strings.ToLower(os.Getenv("MATRISMS_LOG_LEVEL")); lvl != "" {
		switch lvl {
		case "trace":
			zerolog.SetGlobalLevel(zerolog.TraceLevel)
		case "debug":
			zerolog.SetGlobalLevel(zerolog.DebugLevel)
		case "info":
			zerolog.SetGlobalLevel(zerolog.InfoLevel)
		case "warn":
			zerolog.SetGlobalLevel(zerolog.WarnLevel)
		case "error":
			zerolog.SetGlobalLevel(zerolog.ErrorLevel)
		}
	}

	// Ensure ./data directory exists for the SQLite DB, salt, and passphrase.
	dataDir := filepath.Join(".", "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		bridge.Log.Warn().Err(err).Str("path", dataDir).Msg("Failed to ensure data directory exists")
	}

	sc.DB = &SMSAccountQuery{DB: bridge.DB}
	ctx := context.Background()
	if err := sc.DB.CreateTable(ctx); err != nil {
		bridge.Log.Error().Err(err).Msg("Failed to create sms_accounts tables - bridge initialization failed")
		panic(fmt.Errorf("database initialization failed: %w", err))
	}

	sc.Webhook = NewWebhookServer(sc)

	initCtx, cancel := context.WithCancel(context.Background())
	sc.initCancel = cancel
	_ = initCtx

	sc.Bridge.Commands.(*commands.Processor).AddHandlers(sc.createCommands()...)
}

func (sc *SMSConnector) Start(ctx context.Context) error {
	sc.Bridge.Log.Info().Msg("VoIP.ms SMS connector starting...")
	sc.setupLogo(ctx)
	if sc.Config.Webhook.Enabled {
		if err := sc.Webhook.Start(); err != nil {
			return fmt.Errorf("failed to start SMS webhook listener: %w", err)
		}
	}
	return nil
}

// Stop gracefully shuts down the connector; per-login pollers are stopped by
// the framework via SMSClient.Stop.
func (sc *SMSConnector) Stop() {
	sc.Bridge.Log.Info().Msg("VoIP.ms SMS connector stopping...")
	if sc.Webhook != nil {
		sc.Webhook.Stop()
	}
	if sc.initCancel != nil {
		sc.initCancel()
	}
}

func (sc *SMSConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		DisappearingMessages: false,
		AggressiveUpdateInfo: false,
	}
}

func (sc *SMSConnector) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	number := strings.TrimPrefix(string(ghost.ID), "sms:")
	name := common.FormatPhone(number)
	normalized := common.NormalizePhone(number)
	for _, client := range sc.activeClients() {
		if client.OwnsDID(normalized) {
			// Without double puppeting, the user's own sent messages fall
			// back to this ghost — make it unmistakably "you".
			name = fmt.Sprintf("Me (%s)", common.FormatPhone(number))
			break
		}
		if contact := client.ContactName(ctx, normalized); contact != "" {
			name = contact
			break
		}
	}
	return &bridgev2.UserInfo{
		Name:        &name,
		Identifiers: []string{fmt.Sprintf("tel:+%s", number)},
	}, nil
}

// contactNameFor resolves a peer's phonebook name via the portal's login,
// falling back to any active client. Returns "" when unknown.
func (sc *SMSConnector) contactNameFor(ctx context.Context, userLogin *bridgev2.UserLogin, peer string) string {
	if userLogin != nil {
		if client, ok := userLogin.Client.(*SMSClient); ok {
			return client.ContactName(ctx, peer)
		}
	}
	for _, client := range sc.activeClients() {
		if name := client.ContactName(ctx, peer); name != "" {
			return name
		}
	}
	return ""
}

// GetChatInfo builds room info for the two portal kinds:
//   - "did:<did>"       → the per-DID space grouping that number's chats
//   - "sms:<did>:<peer>" → one conversation room, parented into the space
func (sc *SMSConnector) GetChatInfo(ctx context.Context, portal *bridgev2.Portal, userLogin *bridgev2.UserLogin, portalKey networkid.PortalKey) (*bridgev2.ChatInfo, error) {
	if did, ok := common.ParseDIDPortalID(portalKey.ID); ok {
		spaceName := fmt.Sprintf("SMS %s", common.FormatPhone(did))
		topic := fmt.Sprintf("Text conversations of your VoIP.ms number %s", common.FormatPhone(did))
		members := &bridgev2.ChatMemberList{IsFull: true}
		if userLogin != nil {
			members.Members = []bridgev2.ChatMember{{
				EventSender: bridgev2.EventSender{IsFromMe: true},
				Membership:  "join",
			}}
		}
		return &bridgev2.ChatInfo{
			Name:    &spaceName,
			Topic:   &topic,
			Avatar:  sc.logoAvatar(),
			Type:    ptr.Ptr(database.RoomTypeSpace),
			Members: members,
		}, nil
	}

	did, peer, ok := common.ParsePortalID(portalKey.ID)
	if !ok {
		return nil, fmt.Errorf("unrecognized portal ID %q", portalKey.ID)
	}

	roomName := common.FormatPhone(peer)
	if contact := sc.contactNameFor(ctx, userLogin, peer); contact != "" {
		roomName = contact
	}
	topic := fmt.Sprintf("SMS conversation with %s via your VoIP.ms number %s", common.FormatPhone(peer), common.FormatPhone(did))

	ghostID := common.PhoneToGhostID(peer)
	members := &bridgev2.ChatMemberList{
		IsFull: true,
		Members: []bridgev2.ChatMember{
			{
				EventSender: bridgev2.EventSender{Sender: ghostID},
				Membership:  "join",
			},
		},
	}
	if userLogin != nil {
		members.Members = append(members.Members, bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{IsFromMe: true},
			Membership:  "join",
		})
	}

	return &bridgev2.ChatInfo{
		Name:     &roomName,
		Topic:    &topic,
		Type:     ptr.Ptr(database.RoomTypeDM),
		ParentID: ptr.Ptr(common.DIDPortalIDFor(did)),
		Members:  members,
	}, nil
}

func (sc *SMSConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID == "" {
		flowID = LoginFlowIDAPIPassword
	}
	if flowID != LoginFlowIDAPIPassword {
		return nil, fmt.Errorf("unknown login flow %q", flowID)
	}
	return &SMSLoginProcess{user: user, connector: sc}, nil
}

func (sc *SMSConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "VoIP.ms API credentials",
		Description: "Log in with your VoIP.ms account email and API password (Main Menu → SOAP & REST/JSON API). Remember to enable the API and whitelist this server's IP there.",
		ID:          LoginFlowIDAPIPassword,
	}}
}

func (sc *SMSConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		// Without this factory the framework can't deserialize UserLogin
		// metadata from the DB at startup, and LoadUserLogin fails with
		// "no API username metadata" after a bridge restart.
		UserLogin: func() any { return &SMSLoginMetadata{} },
		Message:   func() any { return &SMSMessageMetadata{} },
	}
}

func (sc *SMSConnector) GetBridgeInfoVersion() (int, int) {
	return 1, 1
}
