package connector

import (
	"context"
	_ "embed"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// voipmsLogoPNG is the VoIP.ms checkmark logo, shown as the bot avatar, the
// per-DID space avatar, and the bridge-info network icon.
//
//go:embed voipms-logo.png
var voipmsLogoPNG []byte

// logoAvatarID versions the embedded logo: bump the suffix when the file
// changes so bridgev2 re-uploads portal avatars.
const logoAvatarID = "voipms-logo-v1"

const logoMXCKey = "logo_mxc:" + logoAvatarID

// logoAvatar returns the embedded logo as a bridgev2 avatar for portals;
// the framework uploads and caches it keyed by AvatarID.
func (sc *SMSConnector) logoAvatar() *bridgev2.Avatar {
	return &bridgev2.Avatar{
		ID: networkid.AvatarID(logoAvatarID),
		Get: func(ctx context.Context) ([]byte, error) {
			return voipmsLogoPNG, nil
		},
	}
}

// setupLogo uploads the embedded logo to the media repo (once — the mxc URI
// is cached in the DB), publishes it as the bridge-info network icon, and
// sets it as the bot's avatar unless the config picks one explicitly.
func (sc *SMSConnector) setupLogo(ctx context.Context) {
	mxc, err := sc.DB.GetKV(ctx, logoMXCKey)
	if err != nil {
		sc.Bridge.Log.Warn().Err(err).Msg("Failed to read cached logo mxc URI")
	}
	if mxc == "" {
		uploaded, _, err := sc.Bridge.Bot.UploadMedia(ctx, "", voipmsLogoPNG, "voipms-logo.png", "image/png")
		if err != nil {
			sc.Bridge.Log.Warn().Err(err).Msg("Failed to upload bridge logo; avatars stay unset")
			return
		}
		mxc = string(uploaded)
		if err := sc.DB.SetKV(ctx, logoMXCKey, mxc); err != nil {
			sc.Bridge.Log.Warn().Err(err).Msg("Failed to cache logo mxc URI")
		}
	}
	sc.networkIcon.Store(mxc)

	// An explicit appservice.bot.avatar in the bridge config wins.
	if mxconn, ok := sc.Bridge.Matrix.(*matrix.Connector); ok && mxconn.Config.AppService.Bot.Avatar != "" {
		return
	}
	if err := sc.Bridge.Bot.SetAvatarURL(ctx, id.ContentURIString(mxc)); err != nil {
		sc.Bridge.Log.Warn().Err(err).Msg("Failed to set bot avatar")
	} else {
		sc.Bridge.Log.Debug().Str("mxc", mxc).Msg("Bot avatar set to bridge logo")
	}
}
