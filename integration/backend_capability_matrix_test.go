package integration_test

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// assertLiveCapabilityMatrix runs the #1592 Phase 4 PR8 capability matrix against
// a REAL, running sandbox backend (docker or ssh) — the availability-gated half
// of the parity proof. The host-side suite
// (session.TestBackendCapabilityParity) asserts the descriptor each backend
// SELF-REPORTS; this asserts the real backend actually SERVICES that descriptor
// over the wire, so `make backend-docker-roundtrip` / `backend-ssh-roundtrip`
// exercise the full matrix and not just the happy-path round-trip.
//
// It proves, for a live instance:
//   - Descriptor parity: a remote workspace, every optional capability TRUE (no
//     locality special-case, no unsupported bit) — the same end-state the host
//     matrix pins, but read off the REAL provisioned backend.
//   - Attach + InteractiveInput serviced: typed input reaches the sandbox agent
//     and echoes back over the WS PTY stream the client attaches on (a remote
//     workspace attaches client-side over that stream, dispatched on
//     Capabilities().Workspace — never through backend.Attach).
//   - Preview + liveness serviced over the wire.
//
// Archive + Recover — the two capabilities that need a durable push/pull-branch
// round-trip — are serviced live by the TestDockerBackendArchiveRestore /
// TestSSHBackendArchiveRestore siblings (which also call this helper for the
// descriptor + streaming half), so together the round-trip targets exercise
// every capability in the matrix against a real backend.
func assertLiveCapabilityMatrix(t *testing.T, label string, inst *session.Instance) {
	t.Helper()

	caps := inst.Capabilities()
	if caps.Workspace != session.WorkspaceRemote {
		t.Errorf("%s: live backend Workspace = %v, want WorkspaceRemote (off-box sandbox)", label, caps.Workspace)
	}
	optional := []struct {
		name string
		ok   bool
	}{
		{"Attach", caps.Attach},
		{"Archive", caps.Archive},
		{"Recover", caps.Recover},
		{"TabManagement", caps.TabManagement},
		{"TerminalTab", caps.TerminalTab},
		{"InteractiveInput", caps.InteractiveInput},
	}
	for _, o := range optional {
		if !o.ok {
			t.Errorf("%s: live backend must advertise capability %s at full parity (#1592 Phase 4 end-state)", label, o.name)
		}
	}

	// Service the streamable capabilities over the wire against the live sandbox:
	// Attach (the WS PTY stream) + InteractiveInput (typed bytes reach the agent
	// and echo back). assertDrivable subscribes, types a marker, and waits for the
	// `cat` pane to echo it.
	as := inst.AgentServer()
	assertDrivable(t, as, label+"-capmatrix-ping")

	if _, err := as.Preview(0, false); err != nil {
		t.Errorf("%s: Preview over the wire failed (capability not serviced): %v", label, err)
	}
	if alive, err := as.Alive(); err != nil || !alive {
		t.Errorf("%s: live sandbox reports not Alive (liveness not serviced)", label)
	}
	t.Logf("%s capability matrix ✓ full remote parity advertised + attach/input/preview/liveness serviced over the wire", label)
}
