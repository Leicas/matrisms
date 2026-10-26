package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/Leicas/matrisms/pkg/coordinator"
	"github.com/Leicas/matrisms/pkg/voipms"
)

// newStateTestClient mirrors the flags LoadUserLogin leaves on a freshly
// loaded client: credentials present, poller not yet running.
func newStateTestClient() *SMSClient {
	log := zerolog.Nop()
	ctx, cancel := context.WithCancel(context.Background())
	c := &SMSClient{
		UserLogin:        &bridgev2.UserLogin{Log: log},
		stateCoordinator: coordinator.NewStateCoordinator(nil, &log),
		ctx:              ctx,
		cancel:           cancel,
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

// A mid-run rejection (e.g. ip_not_enabled after the server's public IP
// changed) used to leave the login dead until a bridge restart even once the
// whitelist was fixed: the poll loop kept ticking but nothing ever set
// loggedIn back. The first clean poll must restore it.
func TestPollSuccessRestoresLogin(t *testing.T) {
	c := newStateTestClient()
	c.isConnected.Store(true)
	c.handlePollError(&voipms.APIError{Method: "getSMS", Status: "ip_not_enabled"})
	if c.IsLoggedIn() || c.IsConnected() {
		t.Fatal("a definitive rejection must clear both flags")
	}
	if f := c.LastAuthFailure(); f == nil || f.Status != "ip_not_enabled" {
		t.Fatalf("LastAuthFailure = %+v, want status ip_not_enabled", f)
	}

	c.handlePollSuccess()
	if !c.IsLoggedIn() || !c.IsConnected() {
		t.Fatal("a successful poll must restore the login without a restart")
	}
	if c.LastAuthFailure() != nil {
		t.Fatal("recovery must clear the recorded rejection")
	}
}

// After Disconnect, a poll that was already in flight must not resurrect the
// connected flag.
func TestPollSuccessAfterDisconnectIsIgnored(t *testing.T) {
	c := newStateTestClient()
	c.cancel()
	c.handlePollSuccess()
	if c.IsConnected() {
		t.Fatal("poll success after disconnect must not mark the client connected")
	}
}

// End-to-end through supervise: the startup credential check is rejected
// twice, then VoIP.ms accepts the credentials again (whitelist fixed). The
// supervisor must keep re-checking instead of giving up, and the client must
// come back as logged in + polling with no restart.
func TestSuperviseRecoversFromAuthRejection(t *testing.T) {
	var didCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.FormValue("method") {
		case "getDIDsInfo":
			if didCalls.Add(1) <= 2 {
				_, _ = w.Write([]byte(`{"status":"ip_not_enabled"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"success","dids":[{"did":"5145550100","description":"test","sms_available":"1","sms_enabled":"1","mms_available":"1"}]}`))
		case "getSMS":
			_, _ = w.Write([]byte(`{"status":"no_sms"}`))
		case "getMMS":
			_, _ = w.Write([]byte(`{"status":"no_mms"}`))
		default:
			_, _ = w.Write([]byte(`{"status":"invalid_method"}`))
		}
	}))
	t.Cleanup(srv.Close)

	log := zerolog.Nop()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &SMSClient{
		Main: &SMSConnector{Config: Config{VoIPms: VoIPmsConfig{
			ConnectRetryIntervalSeconds:    1,
			ConnectRetryMaxIntervalSeconds: 1,
		}}},
		UserLogin:        &bridgev2.UserLogin{Log: log},
		DIDs:             []string{"15145550100"},
		API:              voipms.NewClient(srv.URL, "user@example.com", "apipass", &log),
		stateCoordinator: coordinator.NewStateCoordinator(nil, &log),
		ctx:              ctx,
		cancel:           cancel,
	}
	c.Poller = &voipms.Poller{
		Client:       c.API,
		DIDs:         c.DIDs,
		PollInterval: time.Hour,
		CursorLoad:   func(context.Context, string) (string, error) { return "", nil },
		CursorSave:   func(context.Context, string, string) error { return nil },
		OnMessage:    func(context.Context, *voipms.Message) error { return nil },
		OnError:      c.handlePollError,
		OnSuccess:    c.handlePollSuccess,
		Log:          &log,
	}
	c.loggedIn.Store(true)

	go c.supervise()

	sawRejected := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !c.IsLoggedIn() {
			sawRejected = true
		}
		if sawRejected && c.IsLoggedIn() && c.IsConnected() {
			if n := didCalls.Load(); n < 3 {
				t.Fatalf("recovered after only %d credential checks; expected the two rejections first", n)
			}
			if c.LastAuthFailure() != nil {
				t.Fatal("recovery must clear the recorded rejection")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("login not restored after the rejection cleared (saw_rejected=%v logged_in=%v connected=%v checks=%d)",
		sawRejected, c.IsLoggedIn(), c.IsConnected(), didCalls.Load())
}
