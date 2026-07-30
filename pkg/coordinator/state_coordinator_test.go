package coordinator

import (
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/status"
)

func newTestCoordinator() *StateCoordinator {
	log := zerolog.Nop()
	// A nil login makes sendBridgeState a no-op, which is all we need: the
	// assertions are about the computed state, not the Matrix round-trip.
	return NewStateCoordinator(nil, &log)
}

// The VoIP.ms connector reports as "poller". If that name isn't recognized the
// inbox state is never updated and the bridge reports BRIDGE_UNREACHABLE even
// though polling works.
func TestPollerComponentReachesConnected(t *testing.T) {
	sc := newTestCoordinator()

	sc.ReportSimpleEvent("poller", "connection_started", false, "", nil)
	if state, _ := sc.computeBridgeState(); state != status.StateBridgeUnreachable {
		t.Fatalf("before connecting: got %q, want %q", state, status.StateBridgeUnreachable)
	}

	sc.ReportSimpleEvent("poller", "connection_established", true, "", nil)
	sc.ReportSimpleEvent("poller", "idle_started", true, "", nil)

	inbox, _, _ := sc.GetConnectionStates()
	if !inbox.Connected || !inbox.IdleRunning {
		t.Fatalf("inbox state not updated: connected=%v idle=%v", inbox.Connected, inbox.IdleRunning)
	}
	state, errCode := sc.computeBridgeState()
	if state != status.StateConnected {
		t.Errorf("got state %q, want %q", state, status.StateConnected)
	}
	if errCode != "" {
		t.Errorf("got error code %q, want empty", errCode)
	}
}

// "inbox" is the inherited IMAP name for the same component and must keep working.
func TestInboxComponentStillRecognized(t *testing.T) {
	sc := newTestCoordinator()
	sc.ReportSimpleEvent("inbox", "connection_established", true, "", nil)
	sc.ReportSimpleEvent("inbox", "idle_started", true, "", nil)
	if state, _ := sc.computeBridgeState(); state != status.StateConnected {
		t.Errorf("got %q, want %q", state, status.StateConnected)
	}
}

// A mid-connection auth failure must demote the state; the retry loop makes this
// path reachable without a bridge restart.
func TestAuthFailureDemotesConnectedState(t *testing.T) {
	sc := newTestCoordinator()
	sc.ReportSimpleEvent("poller", "connection_established", true, "", nil)
	sc.ReportSimpleEvent("poller", "idle_started", true, "", nil)

	sc.ReportSimpleEvent("poller", "auth_failure", false, SMSConnectionFailed, nil)

	inbox, _, _ := sc.GetConnectionStates()
	if inbox.Connected || inbox.IdleRunning {
		t.Errorf("still connected after auth failure: connected=%v idle=%v", inbox.Connected, inbox.IdleRunning)
	}
	state, errCode := sc.computeBridgeState()
	if state != status.StateBridgeUnreachable {
		t.Errorf("got state %q, want %q", state, status.StateBridgeUnreachable)
	}
	if errCode != SMSConnectionFailed {
		t.Errorf("got error code %q, want %q", errCode, SMSConnectionFailed)
	}
}

// A dead poll loop is a transient disconnect, not a hard failure, and must
// recover once polling restarts.
func TestConnectionLostThenRecovered(t *testing.T) {
	sc := newTestCoordinator()
	sc.ReportSimpleEvent("poller", "connection_established", true, "", nil)
	sc.ReportSimpleEvent("poller", "idle_started", true, "", nil)

	sc.ReportSimpleEvent("poller", "connection_lost", false, SMSPollFailed, nil)
	if state, _ := sc.computeBridgeState(); state != status.StateBridgeUnreachable {
		t.Errorf("after loss: got %q, want %q", state, status.StateBridgeUnreachable)
	}

	sc.ReportSimpleEvent("poller", "connection_established", true, "", nil)
	sc.ReportSimpleEvent("poller", "idle_recovered", true, "", nil)
	if state, _ := sc.computeBridgeState(); state != status.StateConnected {
		t.Errorf("after recovery: got %q, want %q", state, status.StateConnected)
	}
}
