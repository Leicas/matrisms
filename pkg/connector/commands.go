package connector

import (
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/Leicas/matrisms/pkg/common"
	"github.com/Leicas/matrisms/pkg/voipms"
)

var (
	HelpSectionInfo  = commands.HelpSection{Name: "Info", Order: 20}
	HelpSectionAdmin = commands.HelpSection{Name: "Administration", Order: 40}
)

func (sc *SMSConnector) createCommands() []commands.CommandHandler {
	return []commands.CommandHandler{
		&commands.FullHandler{
			Func: fnPing,
			Name: "ping",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "Check if the bridge is alive",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnStatus(ce, sc) },
			Name: "status",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "Show connection status for your VoIP.ms accounts",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnList(ce, sc) },
			Name: "list",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "List bridged phone numbers",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnText(ce, sc) },
			Name: "text",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "Start a conversation with a number",
				Args:        "<number> [message...]",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnPassphrase(ce, sc) },
			Name: "passphrase",
			Help: commands.HelpMeta{
				Section:     HelpSectionAdmin,
				Description: "Show where the database encryption passphrase is stored",
			},
			RequiresAdmin: true,
		},
	}
}

func fnPing(ce *commands.Event) {
	ce.Reply("🏓 Pong! The matrisms bridge is alive.")
}

func getSMSClients(ce *commands.Event) []*SMSClient {
	var out []*SMSClient
	for _, login := range ce.User.GetUserLogins() {
		if client, ok := login.Client.(*SMSClient); ok {
			out = append(out, client)
		}
	}
	return out
}

func fnStatus(ce *commands.Event, sc *SMSConnector) {
	clients := getSMSClients(ce)
	if len(clients) == 0 {
		ce.Reply("No VoIP.ms accounts configured. Use `login` to add one.")
		return
	}
	var b strings.Builder
	for _, c := range clients {
		state := "🔴 disconnected"
		if c.IsConnected() {
			state = "🟢 polling"
		}
		fmt.Fprintf(&b, "**%s** — %s\n", c.APIUsername, state)
		for _, did := range c.DIDs {
			fmt.Fprintf(&b, "  • %s\n", common.FormatPhone(did))
		}
	}
	fmt.Fprintf(&b, "\nPoll interval: %s", sc.Config.VoIPms.EffectivePollInterval())
	if sc.Config.Webhook.Enabled {
		fmt.Fprintf(&b, " · webhook: %s%s", sc.Config.Webhook.EffectiveListenAddress(), sc.Config.Webhook.EffectivePath())
	}
	ce.Reply("%s", b.String())
}

func fnList(ce *commands.Event, sc *SMSConnector) {
	clients := getSMSClients(ce)
	if len(clients) == 0 {
		ce.Reply("No VoIP.ms accounts configured. Use `login` to add one.")
		return
	}
	var b strings.Builder
	b.WriteString("Bridged numbers:\n")
	for _, c := range clients {
		for _, did := range c.DIDs {
			fmt.Fprintf(&b, "  • %s (account %s)\n", common.FormatPhone(did), c.APIUsername)
		}
	}
	ce.Reply("%s", b.String())
}

// fnText opens (or reuses) the portal room for a peer number and optionally
// sends a first message. With multiple DIDs the sending number can be picked
// with from:<number>, otherwise the account's first DID is used.
//
//	!matrisms text 5551234567 hey, this is Antoine
//	!matrisms text from:5559990000 5551234567 hey
func fnText(ce *commands.Event, sc *SMSConnector) {
	clients := getSMSClients(ce)
	if len(clients) == 0 {
		ce.Reply("No VoIP.ms accounts configured. Use `login` first.")
		return
	}
	args := ce.Args
	fromDID := ""
	if len(args) > 0 && strings.HasPrefix(strings.ToLower(args[0]), "from:") {
		fromDID = common.NormalizePhone(args[0][5:])
		args = args[1:]
	}
	if len(args) == 0 {
		ce.Reply("Usage: `text [from:<your-number>] <destination-number> [message...]`")
		return
	}
	peer := common.NormalizePhone(args[0])
	if len(peer) < 11 {
		ce.Reply("❌ %q doesn't look like a valid 10-digit US/Canada number.", args[0])
		return
	}
	message := strings.TrimSpace(strings.Join(args[1:], " "))

	// Pick the client + DID.
	var client *SMSClient
	if fromDID == "" {
		client = clients[0]
		fromDID = client.DIDs[0]
	} else {
		for _, c := range clients {
			for _, did := range c.DIDs {
				if did == fromDID {
					client = c
					break
				}
			}
		}
		if client == nil {
			ce.Reply("❌ %s is not one of your bridged numbers.", common.FormatPhone(fromDID))
			return
		}
	}

	portalKey := networkid.PortalKey{
		ID:       common.PortalIDFor(fromDID, peer),
		Receiver: client.UserLogin.ID,
	}
	portal, err := ce.Bridge.GetPortalByKey(ce.Ctx, portalKey)
	if err != nil {
		ce.Reply("❌ Failed to get portal: %s", err.Error())
		return
	}
	if portal.MXID == "" {
		if err := portal.CreateMatrixRoom(ce.Ctx, client.UserLogin, nil); err != nil {
			ce.Reply("❌ Failed to create room: %s", err.Error())
			return
		}
	}
	ce.Reply("✅ Room ready for %s (from %s): %s", common.FormatPhone(peer), common.FormatPhone(fromDID), portal.MXID)

	if message != "" {
		// Send directly via the API; the poller picks up the outbound echo
		// and bridges it into the room as us.
		parts := voipms.SplitSMS(message)
		for _, part := range parts {
			if _, err := client.API.SendSMS(ce.Ctx, fromDID, peer, part); err != nil {
				ce.Reply("❌ Send failed: %s", humanizeSendError(err).Error())
				return
			}
		}
		client.Poller.TriggerPoll()
		ce.Reply("📤 Sent.")
	}
}

func fnPassphrase(ce *commands.Event, sc *SMSConnector) {
	path, err := getPassphraseFilePath()
	if err != nil {
		ce.Reply("❌ %s", err.Error())
		return
	}
	ce.Reply("The credential-encryption passphrase is read from the `MATRISMS_PASSPHRASE` env var, falling back to `%s`. Keep a copy of it together with your database backups — without it, stored VoIP.ms credentials cannot be decrypted.", path)
}
