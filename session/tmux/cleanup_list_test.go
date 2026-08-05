package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
)

// listFailureTmuxOnPath puts a `tmux` earlier on PATH whose `ls` fails with the
// given exit status and stderr diagnostic. Every OTHER subcommand touches
// touched and exits non-zero, so a test can assert that a refused listing
// reached no further tmux command at all — the sweep stopped, it did not
// merely report.
func listFailureTmuxOnPath(t *testing.T, touched, diagnostic string, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
ls)
  printf '%%s\n' "$LS_DIAGNOSTIC" >&2
  exit %d
  ;;
*)
  : > "$TOUCHED_MARKER"
  exit 97
  ;;
esac
`, exitCode)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LS_DIAGNOSTIC", diagnostic)
	t.Setenv("TOUCHED_MARKER", touched)
}

// TestCleanupSessionsListFailureIsNotAnEmptySessionSet is the #2870 regression
// lock: `af reset` calls CleanupSessions before it deletes worktrees, branches,
// and records, so a FAILED listing read as "there are no sessions" licenses the
// destruction the listing exists to prevent.
//
// `tmux ls` exits 1 for BOTH outcomes, so the exit status alone cannot separate
// them; only the diagnostic can. The table below is measured against tmux 3.4 —
// every string here is one tmux actually emits — and it pins both directions:
//
//   - The two DEFINITIVE absences must still return nil, or `af reset` breaks on
//     every machine that simply has no tmux server running. That is not a
//     hypothetical: the socket-absent (ENOENT) line is the everyday no-server
//     answer, because tmux only prints "no server running on" for a socket it
//     could reach and be REFUSED by.
//   - Everything else must abort, and must abort BEFORE any other tmux command
//     runs.
func TestCleanupSessionsListFailureIsNotAnEmptySessionSet(t *testing.T) {
	cases := []struct {
		name       string
		diagnostic string
		exitCode   int
		wantAbort  bool
	}{
		{
			// A server that exited leaves its socket file behind, so the next
			// connect is REFUSED. Measured: this is what `tmux ls` says after the
			// last session dies.
			name:       "server exited, socket refused",
			diagnostic: "no server running on /tmp/tmux-1000/default",
			exitCode:   1,
		},
		{
			// The socket was never created — a machine where tmux has not run.
			// Measured: this, NOT the line above, is the ordinary "no sessions"
			// answer, so refusing here would break reset for most users.
			name:       "socket absent",
			diagnostic: "error connecting to /tmp/tmux-1000/default (No such file or directory)",
			exitCode:   1,
		},
		{
			// The reported bug: a reachable socket we are not allowed to talk to.
			// Sessions may be running; we cannot see them.
			name:       "permission denied",
			diagnostic: "error connecting to /tmp/tmux-1000/default (Permission denied)",
			exitCode:   1,
			wantAbort:  true,
		},
		{
			name:       "connect timed out",
			diagnostic: "error connecting to /tmp/tmux-1000/default (Connection timed out)",
			exitCode:   1,
			wantAbort:  true,
		},
		{
			// tmux's own socket-directory guards, which never reach connect(2).
			name:       "socket directory rejected",
			diagnostic: "directory /tmp/tmux-1000 has unsafe permissions",
			exitCode:   1,
			wantAbort:  true,
		},
		{
			name:       "socket directory uncreatable",
			diagnostic: "couldn't create directory /tmp/x/tmux-1000 (Not a directory)",
			exitCode:   1,
			wantAbort:  true,
		},
		{
			// A wrapper or policy shim in front of tmux: same exit status, no
			// tmux diagnostic at all.
			name:       "wrapper refusal",
			diagnostic: "tmux-policy: denied by site policy",
			exitCode:   1,
			wantAbort:  true,
		},
		{
			name:       "no diagnostic at all",
			diagnostic: "",
			exitCode:   1,
			wantAbort:  true,
		},
		{
			// A truncated no-server line names no socket, so it is not tmux's
			// answer — it is a prefix match on nothing.
			name:       "no-server line naming no socket",
			diagnostic: "no server running on",
			exitCode:   1,
			wantAbort:  true,
		},
		{
			name:       "non-1 exit status",
			diagnostic: "error connecting to /tmp/tmux-1000/default (No such file or directory)",
			exitCode:   2,
			wantAbort:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			touched := filepath.Join(t.TempDir(), "reached-another-tmux-command")
			listFailureTmuxOnPath(t, touched, tc.diagnostic, tc.exitCode)
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

			err := CleanupSessions(cmd.MakeExecutor())

			if !tc.wantAbort {
				if err != nil {
					t.Fatalf("CleanupSessions error = %v, want nil: %q is tmux's definitive "+
						"no-server answer, so reset must be allowed to proceed", err, tc.diagnostic)
				}
				return
			}
			if err == nil {
				t.Fatalf("CleanupSessions returned nil for %q — an unreadable session list was "+
					"reported as an empty one, which lets `af reset` delete worktrees and prune "+
					"branches with the tmux session set unknown (#2870)", tc.diagnostic)
			}
			if !strings.Contains(err.Error(), "could not list tmux sessions") {
				t.Errorf("error = %q, want it to name the read that failed", err)
			}
			if tc.diagnostic != "" && !strings.Contains(err.Error(), tc.diagnostic) {
				t.Errorf("error = %q, want it to carry tmux's diagnostic %q — `exit status 1` alone "+
					"tells the user nothing to act on", err, tc.diagnostic)
			}
			if _, statErr := os.Stat(touched); !os.IsNotExist(statErr) {
				t.Errorf("a refused listing went on to run another tmux command (stat %s = %v); "+
					"the sweep must stop, not continue on an unknown session set", touched, statErr)
			}
		})
	}
}

// TestCleanupSessionsAcceptsLiveServerWithNoSessions covers the third outcome
// the two branches above are not: tmux ANSWERED and the answer was empty. A
// server configured with `exit-empty off` outlives its last session, and `tmux
// ls` then exits 0 with no output. That is a real empty session set and reset
// may proceed.
func TestCleanupSessionsAcceptsLiveServerWithNoSessions(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\nls) exit 0 ;;\n*) exit 97 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	if err := CleanupSessions(cmd.MakeExecutor()); err != nil {
		t.Fatalf("CleanupSessions error = %v, want nil for a successful listing with no sessions", err)
	}
}

// TestCleanupSessionsAgainstRealTmuxSocket drives the REAL tmux binary rather
// than a script, because the fix turns on strings tmux emits and a fake tmux
// cannot prove those strings are right (#2870: "reproduce it first — an
// unreachable socket via TMUX_TMPDIR").
//
// Both halves matter and neither is redundant with the table above:
//
//   - unreachable socket directory -> CleanupSessions must REFUSE.
//   - readable, empty socket directory (no server) -> it must PROCEED. This is
//     the half that fails if the definitive-absence set is narrowed to the
//     "no server running on" line alone, which is what makes it worth the cost
//     of a real tmux.
//
// Nothing here can reach the developer's live server: TMUX is cleared so the
// ambient session cannot supply a socket, and TMUX_TMPDIR pins resolution
// inside a throwaway directory.
func TestCleanupSessionsAgainstRealTmuxSocket(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux is not installed: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode the unreachable half depends on")
	}

	// A short base path, not t.TempDir(): the socket path lands in sun_path,
	// which is 108 bytes.
	base, err := os.MkdirTemp("", "aftmux")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	sockDir := filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid()))
	if len(filepath.Join(sockDir, "default")) > 100 {
		t.Skipf("TMPDIR is too long for a unix socket path: %s", sockDir)
	}
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// An ambient $TMUX would hand tmux the REAL server's socket and bypass
	// TMUX_TMPDIR entirely. Empty is treated as unset by tmux.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", base)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// Readable and empty: no server, definitively. Reset may proceed.
	if err := CleanupSessions(cmd.MakeExecutor()); err != nil {
		t.Fatalf("CleanupSessions error = %v, want nil — %s holds no tmux server, so this is a "+
			"real empty session set and `af reset` must not be blocked by it", err, sockDir)
	}

	// Unreachable: tmux cannot tell us what is running there.
	if err := os.Chmod(sockDir, 0); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sockDir, 0o700) })

	err = CleanupSessions(cmd.MakeExecutor())
	if err == nil {
		t.Fatal("CleanupSessions returned nil against an unreachable tmux socket — `af reset` " +
			"would go on to delete worktrees and prune branches without knowing what is running (#2870)")
	}
	if !strings.Contains(err.Error(), "could not list tmux sessions") {
		t.Errorf("error = %q, want it to name the read that failed", err)
	}
}

// TestListSessionNamesIsBounded is the #2910 regression. `af doctor --fix`
// re-lists tmux sessions before it removes a stale AF home, so an unbounded
// listing against a server that wedged mid-run hangs the cleanup phase instead
// of refusing a removal it can no longer justify — a hang with no way out but
// ^C, at the moment the user is already cleaning up.
//
// The stalling tmux sleeps in a CHILD, so passing requires the process-group
// kill and WaitDelay, not just a context: a deadline alone leaves Output()
// blocked on the inherited pipe.
func TestListSessionNamesIsBounded(t *testing.T) {
	stallingTmuxOnPath(t)
	shortTmuxTimeout(t, 200*time.Millisecond)

	done := make(chan struct{})
	var names []string
	var err error
	go func() {
		defer close(done)
		names, err = ListSessionNames(cmd.MakeExecutor())
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ListSessionNames never returned against a wedged tmux — an unbounded listing hangs " +
			"`af doctor --fix` in its cleanup phase (#2910)")
	}

	if err == nil {
		t.Fatalf("ListSessionNames returned (%v, nil) on a tripped deadline — a server that did not "+
			"answer has told us nothing about what is running, and must never read as an empty list", names)
	}
	if !errors.Is(err, ErrTmuxTimeout) {
		t.Errorf("error = %v, want ErrTmuxTimeout so callers can tell a wedge from a refusal", err)
	}
	if names != nil {
		t.Errorf("names = %v, want nil alongside the error", names)
	}
}
