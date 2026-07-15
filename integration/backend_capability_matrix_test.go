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
//   - Descriptor parity: a remote workspace with every optional capability TRUE
//     (no locality special-case, no unsupported bit) EXCEPT TabManagement, which
//     is false — the same end-state the host matrix pins, but read off the REAL
//     provisioned backend. TabManagement is serviced below as a rejection: the
//     agent-server has no create-tab RPC, so a live sandbox genuinely cannot
//     spawn one (#1874). It sat in this list as a TRUE bit that nothing here
//     ever exercised over the wire, which is how the contradiction survived.
//   - Attach + InteractiveInput serviced: typed input reaches the sandbox agent
//     and echoes back over the WS PTY stream the client attaches on (a remote
//     workspace attaches client-side over that stream; #1852 deleted the backend
//     attach surface, so there is no other path it could take). Attach is proven
//     here by SERVICING it below, not by a descriptor bit — #1860 deleted the
//     Capabilities.Attach bit, which was true for every backend and read by
//     nothing.
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
		{"Archive", caps.Archive},
		{"Recover", caps.Recover},
		{"TerminalTab", caps.TerminalTab},
		{"InteractiveInput", caps.InteractiveInput},
	}
	for _, o := range optional {
		if !o.ok {
			t.Errorf("%s: live backend must advertise capability %s at full parity (#1592 Phase 4 end-state)", label, o.name)
		}
	}

	// TabManagement is the one bit that must be FALSE off-box, and it is serviced
	// here rather than merely asserted: the live sandbox must actually refuse the
	// spawn. Advertising true while every Add*Tab path needs a daemon-side
	// worktree is exactly the drift #1874 fixed, so pin BOTH halves — if a future
	// agent-server grows a create-tab RPC, both flip together in one change.
	if caps.TabManagement {
		t.Errorf("%s: live backend advertises TabManagement, but tab creation needs a daemon-side worktree an off-box workspace does not have (#1874)", label)
	}
	if _, err := inst.AddShellTab(); err == nil {
		t.Errorf("%s: AddShellTab unexpectedly succeeded on an off-box workspace; if the agent-server now services tab creation, flip TabManagement true with it (#1874)", label)
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
