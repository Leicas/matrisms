package connector

import (
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/Leicas/matrisms/pkg/coordinator"
	"github.com/Leicas/matrisms/pkg/voipms"
)

// newStateTestClient mirrors the flags LoadUserLogin leaves on a freshly
// loaded client: credentials present, poller not yet running.
func newStateTestClient() *SMSClient {
	log := zerolog.Nop()
	c := &SMSClient{
		UserLogin:        &bridgev2.UserLogin{Log: log},
		stateCoordinator: coordinator.NewStateCoordinator(nil, &log),
	}
	c.loggedIn.Store(true)
	return c
}

// Regression: IsLoggedIn used to alias IsConnected, so bridgev2 answered
// every Matrix -> SMS message with "You're not logged in" whenever the
// poller was between reconnect attempts (or had not finished its first
// credential check yet).
func TestLoggedInBeforeFirstConnect(t *testing.T) {
	c := newStateTestClient()
	if !c.IsLoggedIn() {
		t.Fatal("client with stored credentials must report logged in before the poller starts")
	}
	if c.IsConnected() {
		t.Fatal("poller has not started; IsConnected must be false")
	}
}

func TestLoggedInSurvivesPollerDrop(t *testing.T) {
	c := newStateTestClient()
	c.isConnected.Store(true)
	// Poller stops (as connectAndPoll does before supervise retries).
	c.isConnected.Store(false)
	if !c.IsLoggedIn() {
		t.Fatal("losing the poll loop must not log the user out")
	}
}

func TestTransientPollErrorKeepsLogin(t *testing.T) {
	c := newStateTestClient()
	c.isConnected.Store(true)
	c.handlePollError(errors.New("Post https://voip.ms/api/v1/rest.php: 522 origin timeout"))
	if !c.IsLoggedIn() || !c.IsConnected() {
		t.Fatal("a non-auth poll error must leave both flags untouched")
	}
}

func TestAuthErrorLogsOut(t *testing.T) {
	c := newStateTestClient()
	c.isConnected.Store(true)
	c.handlePollError(&voipms.APIError{Status: "invalid_credentials"})
	if c.IsLoggedIn() {
		t.Fatal("a definitive credential rejection must clear IsLoggedIn")
	}
	if c.IsConnected() {
		t.Fatal("a definitive credential rejection must clear IsConnected")
	}
}
