package connector

import (
	"testing"
	"time"
)

func TestConnectRetryDefaults(t *testing.T) {
	var v VoIPmsConfig // unset config = defaults
	if !v.ConnectRetryEnabled() {
		t.Error("retries should be enabled by default")
	}
	if got, want := v.EffectiveConnectRetryInterval(), 30*time.Second; got != want {
		t.Errorf("initial interval = %v, want %v", got, want)
	}
	if got, want := v.EffectiveConnectRetryMaxInterval(), 10*time.Minute; got != want {
		t.Errorf("max interval = %v, want %v", got, want)
	}
	if v.ConnectMaxRetries != 0 {
		t.Errorf("max retries = %d, want 0 (unlimited)", v.ConnectMaxRetries)
	}
}

func TestConnectRetryConfigured(t *testing.T) {
	v := VoIPmsConfig{
		ConnectRetryIntervalSeconds:    5,
		ConnectRetryMaxIntervalSeconds: 120,
	}
	if got, want := v.EffectiveConnectRetryInterval(), 5*time.Second; got != want {
		t.Errorf("initial interval = %v, want %v", got, want)
	}
	if got, want := v.EffectiveConnectRetryMaxInterval(), 2*time.Minute; got != want {
		t.Errorf("max interval = %v, want %v", got, want)
	}
}

// -1 restores the pre-retry behaviour of giving up on the first failure.
func TestConnectRetryDisabled(t *testing.T) {
	v := VoIPmsConfig{ConnectRetryIntervalSeconds: -1}
	if v.ConnectRetryEnabled() {
		t.Error("retries should be disabled when the interval is -1")
	}
}

// A max below the initial interval would make the backoff shrink; clamp it.
func TestConnectRetryMaxNeverBelowInitial(t *testing.T) {
	v := VoIPmsConfig{
		ConnectRetryIntervalSeconds:    60,
		ConnectRetryMaxIntervalSeconds: 10,
	}
	if got, want := v.EffectiveConnectRetryMaxInterval(), 60*time.Second; got != want {
		t.Errorf("max interval = %v, want %v (the initial interval)", got, want)
	}
}

// The example config must carry every key upgradeConfig copies, or a fresh
// config silently loses the retry settings.
func TestExampleConfigDocumentsRetryKeys(t *testing.T) {
	for _, key := range []string{
		"connect_retry_interval_seconds",
		"connect_retry_max_interval_seconds",
		"connect_max_retries",
	} {
		if !contains(ExampleConfig, key) {
			t.Errorf("example-config.yaml is missing %q", key)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
