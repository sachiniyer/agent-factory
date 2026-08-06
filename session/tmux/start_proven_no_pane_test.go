package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// #2985: a create that fails BEFORE new-session leaves nothing running, and the
// teardown gate needs to know that — on exactly the machines where asking tmux
// cannot be answered.
//
// What makes the predicate delicate is how nearly-right the wrong ones are. The
// obvious signal, "Start did not succeed", is true of a create whose pane spawned
// fine and whose later setup failed; acting on THAT skips the liveness gate for a
// live agent and deletes its worktree. So these tests pin the boundary from both
// sides: the proof is claimed where absence was established, and refused
// everywhere else.

// determinateAbsence is tmux's real "no such session" answer: exit 1 carrying the
// exact diagnostic. A bare exit 1 is NOT this — wrapper and policy failures share
// that status while the session is very much alive — and the strict probe the
// exemption rests on rejects it, which is the whole point of building the fixture
// out of a real ExitError instead of a formatted string.
func determinateAbsence(t *testing.T, name string) error {
	t.Helper()
	_, err := exec.Command("sh", "-c", fmt.Sprintf("printf \"can't find session: %s\\n\" >&2; exit 1", name)).Output()
	if err == nil {
		t.Fatal("fixture: expected the shell to exit 1")
	}
	return err
}

// answeredAbsent routes has-session to a determinate absence answer.
func answeredAbsent(t *testing.T, c *exec.Cmd, name string) (bool, error) {
	t.Helper()
	for _, a := range c.Args {
		if a == "has-session" {
			return true, determinateAbsence(t, name)
		}
	}
	return false, nil
}

// TestStart_EnvironmentFailureBeforeSpawn_ProvesNoPane is the reported case: the
// update-environment read fails, which happens after the name is proven absent
// and before new-session runs.
//
// The failure has to be one importClientEnvironmentArgs actually raises. "No
// server running" is NOT it — that is the ordinary first-session case and it
// returns cleanly, which is why a fixture built on it sails past this point and
// spawns instead. An unreachable server that answers with something else does
// fail, and so does a read that times out; both leave the name exactly as the
// probe found it, because show-options is read-only.
func TestStart_EnvironmentFailureBeforeSpawn_ProvesNoPane(t *testing.T) {
	execu := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if absent, err := answeredAbsent(t, c, "af_2985_env"); absent {
				return err
			}
			return fmt.Errorf("tmux is unavailable")
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			// has-session must answer here too: the exemption re-probes STRICTLY
			// before it will claim anything, and that probe reads Output.
			if absent, err := answeredAbsent(t, c, "af_2985_env"); absent {
				return nil, err
			}
			return nil, fmt.Errorf("permission denied talking to the tmux server")
		},
	}
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_2985_env", "claude", NewMockPtyFactory(t), execu)

	err := session.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start must fail when the session environment cannot be read")
	}
	if !session.ProvenNoPane() {
		t.Fatalf("a Start that failed before new-session — with the name proven absent — must record that "+
			"no pane exists, or the teardown gate retains the failed create as a tombstone (#2985). err: %v", err)
	}
}

// TestStart_AlreadyExists_ProvesNothing is the case a "Start failed" predicate
// gets wrong in the dangerous direction: the name is TAKEN, so something may well
// be running in that worktree — a leftover from a previous run, most likely.
func TestStart_AlreadyExists_ProvesNothing(t *testing.T) {
	execu := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			for _, a := range c.Args {
				if a == "has-session" {
					return nil // answered: the session EXISTS
				}
			}
			return nil
		},
	}
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_2985_exists", "claude", NewMockPtyFactory(t), execu)

	err := session.Start(t.TempDir())
	if !errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("expected the already-exists refusal, got: %v", err)
	}
	if session.ProvenNoPane() {
		t.Fatal("a name that is already taken proves the OPPOSITE of an empty one: something may be " +
			"running in that worktree, and the teardown gate must still establish liveness (#2985)")
	}
}

// TestStart_UnknownProbe_ProvesNothing: a has-session that never answered leaves
// the name's occupancy unknown, which is not a proof of absence.
func TestStart_UnknownProbe_ProvesNothing(t *testing.T) {
	shortTmuxTimeout(t, 100*time.Millisecond)

	execu := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error {
			time.Sleep(2 * time.Second)
			return fmt.Errorf("wedged tmux server never answered")
		},
	}
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_2985_wedged", "claude", NewMockPtyFactory(t), execu)

	if err := session.Start(t.TempDir()); err == nil {
		t.Fatal("a wedged has-session probe must fail Start")
	}
	if session.ProvenNoPane() {
		t.Fatal("an unanswered probe is not an absent name — claiming no pane here would skip the " +
			"liveness gate on a session nobody could see (#2985)")
	}
}

// TestStart_SpawnSucceededThenFailed_ProvesNothing is the criterion a wrong
// predicate fails, and the one the previous attempt on this issue was caught by.
//
// Same fixture as the wedged-readiness test above it in this package: has-session
// answers "gone" so Start proceeds, the spawn SUCCEEDS through the pty factory —
// a detached session now exists and its pane may be running in the worktree — and
// only then does the server wedge. "Start did not succeed" is true here; "no pane
// exists" must not be, or the teardown gate is skipped for a live agent.
func TestStart_SpawnSucceededThenFailed_ProvesNothing(t *testing.T) {
	shortTmuxTimeout(t, 150*time.Millisecond)

	execu := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if absent, err := answeredAbsent(t, c, "af_2985_spawned"); absent {
				return err
			}
			time.Sleep(2 * time.Second)
			return fmt.Errorf("wedged tmux server never answered")
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if len(c.Args) >= 2 && c.Args[1] == "show-options" {
				return nil, fmt.Errorf("no server running")
			}
			return nil, fmt.Errorf("wedged tmux server never answered")
		},
	}
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_2985_spawned", "claude", NewMockPtyFactory(t), execu)

	err := session.Start(t.TempDir())
	if err == nil {
		t.Fatal("a Start whose readiness poll never sees its session must fail")
	}
	if session.ProvenNoPane() {
		t.Fatal("new-session RAN — the spawn succeeded and only the readiness poll failed — so a pane " +
			"may be live. Claiming no pane here is exactly the unsound predicate that would delete a " +
			"live agent's worktree (#2985 acceptance criterion 2)")
	}
}

// TestStart_ProbeDeniedNotAbsent_ProvesNothing is the P1 on this change: the gate
// at the top of Start reads probeSession, which collapses EVERY non-timeout
// execution failure into absence. A wrapper or socket policy that denies access
// while the server and its pane are alive therefore looks like a free name — and
// latching on that would skip the teardown's liveness gate and delete the worktree
// under a running agent.
func TestStart_ProbeDeniedNotAbsent_ProvesNothing(t *testing.T) {
	execu := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error {
			// Exit 1 with no "can't find session" diagnostic: the shape a policy
			// denial takes, and indistinguishable from absence to the lossy probe.
			return fmt.Errorf("exit status 1")
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("permission denied talking to the tmux server")
		},
	}
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_2985_denied", "claude", NewMockPtyFactory(t), execu)

	if err := session.Start(t.TempDir()); err == nil {
		t.Fatal("Start must fail when the environment cannot be read")
	}
	if session.ProvenNoPane() {
		t.Fatal("a denied has-session is not an absent one: the lossy probe reports both the same way, " +
			"so claiming no pane here would let a teardown delete a live agent's worktree")
	}
}
