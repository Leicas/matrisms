package connector

import (
	_ "embed"
	"time"

	up "go.mau.fi/util/configupgrade"
)

//go:embed example-config.yaml
var ExampleConfig string

type Config struct {
	// Top-level blocks to match example-config.yaml structure
	VoIPms  VoIPmsConfig  `yaml:"voipms"`
	Webhook WebhookConfig `yaml:"webhook"`
	Logging LoggingConfig `yaml:"logging"`
}

// VoIPmsConfig is the on-disk shape of the voipms: block. Credentials are
// NOT configured here — they are provided per-user at login time via
// `!matrisms login` and stored encrypted in the bridge database.
type VoIPmsConfig struct {
	// BaseURL of the VoIP.ms REST API. Only change this for testing.
	BaseURL string `yaml:"base_url"`

	// PollIntervalSeconds is how often each account polls getSMS/getMMS for
	// new messages. VoIP.ms has no push API (other than the SMS URL callback,
	// see the webhook block), so polling is the baseline delivery mechanism.
	// Default 30. Don't go too low — VoIP.ms throttles aggressive API users.
	PollIntervalSeconds int `yaml:"poll_interval_seconds"`

	// StartupBackfillHours controls how far back the first poll of a freshly
	// logged-in account looks for messages. Default 24. Set 0 to only bridge
	// messages that arrive after login.
	StartupBackfillHours int `yaml:"startup_backfill_hours"`

	// MaxUploadBytes caps a single inbound MMS media upload to Matrix.
	// Default 25 MiB. Set 0 to disable the check.
	MaxUploadBytes int `yaml:"max_upload_bytes"`

	// PhonebookNames controls whether the VoIP.ms phonebook is used to name
	// ghosts and rooms after contacts. Default true (nil = unset = true).
	PhonebookNames *bool `yaml:"phonebook_names"`

	// PhonebookRefreshMinutes is how long phonebook contact names are cached
	// before being re-fetched. Default 15.
	PhonebookRefreshMinutes int `yaml:"phonebook_refresh_minutes"`

	// ConvertReactions turns inbound reaction-fallback texts ("Reacted 😂 to
	// \"…\"", tapbacks, « A réagi 😂 à … ») into real Matrix reactions on the
	// quoted message. Default true (nil = unset = true).
	ConvertReactions *bool `yaml:"convert_reactions"`

	// ReactionFallbackTemplate renders reactions sent from Matrix as SMS.
	// Placeholders: {emoji}, {message}. Default: Reacted {emoji} to "{message}"
	ReactionFallbackTemplate string `yaml:"reaction_fallback_template"`

	// ReactionRemoveFallbackTemplate renders reaction removals sent from
	// Matrix as SMS. Default: Removed {emoji} from "{message}"
	ReactionRemoveFallbackTemplate string `yaml:"reaction_remove_fallback_template"`

	// UnscrambleSegments repairs long inbound messages whose segments VoIP.ms
	// reassembled in arrival order instead of sequence order. Conservative:
	// only rewrites bodies with unambiguous structural evidence of scrambling.
	// Default true (nil = unset = true).
	UnscrambleSegments *bool `yaml:"unscramble_segments"`
}

const DefaultMaxUploadBytes = 25 * 1024 * 1024 // 25 MiB

// EffectivePollInterval returns the poll interval with the default applied.
func (v VoIPmsConfig) EffectivePollInterval() time.Duration {
	if v.PollIntervalSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(v.PollIntervalSeconds) * time.Second
}

// EffectiveBaseURL returns the API base URL with the default applied.
func (v VoIPmsConfig) EffectiveBaseURL() string {
	if v.BaseURL == "" {
		return "https://voip.ms/api/v1/rest.php"
	}
	return v.BaseURL
}

// EffectiveBackfill returns the startup backfill window.
func (v VoIPmsConfig) EffectiveBackfill() time.Duration {
	if v.StartupBackfillHours < 0 {
		return 0
	}
	if v.StartupBackfillHours == 0 {
		return 24 * time.Hour
	}
	return time.Duration(v.StartupBackfillHours) * time.Hour
}

// EffectivePhonebookNames reports whether phonebook naming is enabled.
func (v VoIPmsConfig) EffectivePhonebookNames() bool {
	return v.PhonebookNames == nil || *v.PhonebookNames
}

// EffectivePhonebookRefresh returns the phonebook cache TTL.
func (v VoIPmsConfig) EffectivePhonebookRefresh() time.Duration {
	if v.PhonebookRefreshMinutes <= 0 {
		return 15 * time.Minute
	}
	return time.Duration(v.PhonebookRefreshMinutes) * time.Minute
}

// EffectiveConvertReactions reports whether reaction-fallback conversion is on.
func (v VoIPmsConfig) EffectiveConvertReactions() bool {
	return v.ConvertReactions == nil || *v.ConvertReactions
}

// EffectiveUnscrambleSegments reports whether scrambled-segment repair is on.
func (v VoIPmsConfig) EffectiveUnscrambleSegments() bool {
	return v.UnscrambleSegments == nil || *v.UnscrambleSegments
}

// EffectiveReactionTemplate returns the outbound reaction SMS template.
func (v VoIPmsConfig) EffectiveReactionTemplate() string {
	if v.ReactionFallbackTemplate == "" {
		return `Reacted {emoji} to "{message}"`
	}
	return v.ReactionFallbackTemplate
}

// EffectiveReactionRemoveTemplate returns the outbound reaction-removal SMS template.
func (v VoIPmsConfig) EffectiveReactionRemoveTemplate() string {
	if v.ReactionRemoveFallbackTemplate == "" {
		return `Removed {emoji} from "{message}"`
	}
	return v.ReactionRemoveFallbackTemplate
}

// EffectiveMaxUploadBytes returns the MMS upload cap.
func (v VoIPmsConfig) EffectiveMaxUploadBytes() int {
	if v.MaxUploadBytes == 0 {
		return DefaultMaxUploadBytes
	}
	if v.MaxUploadBytes < 0 {
		return 0
	}
	return v.MaxUploadBytes
}

// WebhookConfig is the on-disk shape of the webhook: block. When enabled, the
// bridge runs a small HTTP listener that VoIP.ms can call the instant an SMS
// arrives (Main Menu → DID Settings → SMS URL Callback). Polling stays on as
// a safety net; the webhook just makes delivery instant and lets you raise
// the poll interval.
type WebhookConfig struct {
	// Enabled turns the listener on.
	Enabled bool `yaml:"enabled"`

	// ListenAddress is the host:port to bind, e.g. "0.0.0.0:29332".
	ListenAddress string `yaml:"listen_address"`

	// Path is the URL path VoIP.ms will call. Default "/sms".
	Path string `yaml:"path"`

	// Secret is an optional token that must be present as the `token` query
	// parameter on callback requests. Strongly recommended if the listener is
	// reachable from the internet: set the callback URL to
	// https://your.host/sms?token=<secret>&from={FROM}&message={MESSAGE}&id={ID}&to={TO}
	Secret string `yaml:"secret"`
}

// EffectivePath returns the callback path with the default applied.
func (w WebhookConfig) EffectivePath() string {
	if w.Path == "" {
		return "/sms"
	}
	return w.Path
}

// EffectiveListenAddress returns the bind address with the default applied.
func (w WebhookConfig) EffectiveListenAddress() string {
	if w.ListenAddress == "" {
		return "0.0.0.0:29332"
	}
	return w.ListenAddress
}

type LoggingConfig struct {
	// When true, redact PII (phone numbers, message bodies) from logs.
	Sanitized bool `yaml:"sanitized"`
}

func upgradeConfig(helper up.Helper) {
	// Copy all keys that exist in the embedded example (pkg/connector/example-config.yaml)

	helper.Copy(up.Str, "voipms", "base_url")
	helper.Copy(up.Int, "voipms", "poll_interval_seconds")
	helper.Copy(up.Int, "voipms", "startup_backfill_hours")
	helper.Copy(up.Int, "voipms", "max_upload_bytes")
	helper.Copy(up.Bool, "voipms", "phonebook_names")
	helper.Copy(up.Int, "voipms", "phonebook_refresh_minutes")
	helper.Copy(up.Bool, "voipms", "convert_reactions")
	helper.Copy(up.Str, "voipms", "reaction_fallback_template")
	helper.Copy(up.Str, "voipms", "reaction_remove_fallback_template")
	helper.Copy(up.Bool, "voipms", "unscramble_segments")

	helper.Copy(up.Bool, "webhook", "enabled")
	helper.Copy(up.Str, "webhook", "listen_address")
	helper.Copy(up.Str, "webhook", "path")
	helper.Copy(up.Str, "webhook", "secret")

	helper.Copy(up.Bool, "logging", "sanitized")
}

func (sc *SMSConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &sc.Config, &up.StructUpgrader{
		SimpleUpgrader: up.SimpleUpgrader(upgradeConfig),
		Blocks:         [][]string{},
		Base:           ExampleConfig,
	}
}
