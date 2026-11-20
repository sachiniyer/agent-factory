package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
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

// pinTmuxServerProbe fixes the answer to "is a tmux server alive for this uid?"
// for one test.
//
// It has to be pinned, not observed: since #2875 the ENOENT diagnostic is
// definitive only when NO server could own an unlinked socket, so the real probe
// reads the host process table and a developer box with tmux running gives a
// different answer from a container with none. A test that did not pin it would
// assert one thing locally and another in CI.
//
// An alive server is staged as positively identified and unresolved against
// the missing socket: a live server that may own it, which the refusal names
// and offers its signal (#7541).
func pinTmuxServerProbe(t *testing.T, alive bool) {
	t.Helper()
	var pids []int
	if alive {
		pids = []int{4242}
		pinPossibleOwnerServers(t, pids...)
	}
	t.Cleanup(PinServerProbeForTest(pids...))
}

// pinPossibleOwnerServers stages pids as positively identified tmux servers
// whose binding to the missing socket could not be resolved, so each one
// stays a possible owner and is never signalled.
func pinPossibleOwnerServers(t *testing.T, pids ...int) {
	t.Helper()
	pinServerIdentityForTest(t, pids...)
	pinSocketBindingForTest(t, func(int, string) (socketBinding, string) { return socketBindingUnresolved, "" })
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
			// No server anywhere: the world in which tmux's diagnostics are the
			// only evidence. The ENOENT-with-a-live-server case is its own test.
			pinTmuxServerProbe(t, false)
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

	// The throwaway TMUX_TMPDIR holds no server, but the HOST may well be
	// running tmux (this was written on a box with several), and since #2875 a
	// live server makes ENOENT ambiguous. Pin the answer to the world this test
	// is describing: nothing else is running.
	pinTmuxServerProbe(t, false)

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

// TestSocketAbsentWithALiveServerIsNotAnEmptySessionSet is the #2875 regression:
// the hole left in #2870's own fix.
//
// tmux(1) notes that a socket removed by accident can be recreated by signalling
// the server — which is to say the SERVER SURVIVES ITS SOCKET. Reproduced by
// hand: a session and its `sleep 300` pane stayed alive after `rm -f` of the
// socket, and `tmux ls` answered exactly the ENOENT diagnostic that #2870
// classified as definitive absence. `af reset` would then have deleted worktrees
// and pruned branches with agents running — the very thing #2870 was for.
//
// Not hypothetical: systemd-tmpfiles clears /tmp of files untouched for 10 days
// by default, and af tmux sessions routinely outlive that.
//
// Both directions are pinned, because the naive fix (drop the ENOENT branch)
// breaks `af reset` on every machine that simply has no tmux server: a server
// that EXITS leaves its socket behind, so a dead server answers ECONNREFUSED,
// and ENOENT is the ordinary no-server answer.
func TestSocketAbsentWithALiveServerIsNotAnEmptySessionSet(t *testing.T) {
	const enoent = "error connecting to /tmp/tmux-1000/default (No such file or directory)"
	const refused = "no server running on /tmp/tmux-1000/default"

	for _, tc := range []struct {
		name        string
		diagnostic  string
		serverAlive bool
		wantAbort   bool
	}{
		{"socket absent, no server anywhere", enoent, false, false},
		{"socket absent while a server is ALIVE", enoent, true, true},
		// ECONNREFUSED is self-sufficient: the socket exists and refused us, so
		// nothing is listening on it whatever else is running.
		{"connection refused, no server", refused, false, false},
		{"connection refused, another server alive elsewhere", refused, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			touched := filepath.Join(t.TempDir(), "reached-another-tmux-command")
			pinTmuxServerProbe(t, tc.serverAlive)
			listFailureTmuxOnPath(t, touched, tc.diagnostic, 1)
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

			err := CleanupSessions(cmd.MakeExecutor())

			if !tc.wantAbort {
				if err != nil {
					t.Fatalf("CleanupSessions error = %v, want nil — %q with serverAlive=%v is a real "+
						"empty session set, and refusing here blocks reset on ordinary machines",
						err, tc.diagnostic, tc.serverAlive)
				}
				return
			}
			if err == nil {
				t.Fatalf("CleanupSessions returned nil for %q while a tmux server is running — the "+
					"server outlives an unlinked socket, so this is an unreadable session set, not an "+
					"empty one, and `af reset` would delete worktrees with agents live (#2875)", tc.diagnostic)
			}
			if !strings.Contains(err.Error(), "a tmux server is running") {
				t.Errorf("error = %q, want it to explain why a missing socket is not proof of absence — "+
					"otherwise the refusal reads as a contradiction of tmux's own diagnostic", err)
			}
			if _, statErr := os.Stat(touched); !os.IsNotExist(statErr) {
				t.Errorf("the sweep continued to another tmux command after refusing (stat %s = %v)", touched, statErr)
			}
		})
	}
}

// TestSocketAbsentRefusalNamesAWorkingRecovery locks the recovery hint against
// the way the obvious one fails (Codex on #2956).
//
// The first version said `pkill -USR1 -x -u "$(id -u)" tmux`. tmux renames the
// server task to "tmux: server", and pkill's -x is an EXACT command-name match,
// so that command matches nothing at all — verified with pgrep, which shares the
// matching: `pgrep -x -u $(id -u) tmux` returned nothing on a box with four live
// servers, while `-x "tmux: server"` matched all four. A hint that silently
// signals nothing is worse than no hint: the user runs it, believes they acted,
// and reset keeps refusing.
//
// Dropping -x is not the fix. SIGUSR1's default disposition is TERMINATE, so a
// loose name match would kill any unrelated process merely named like tmux.
// Naming the PID is exact and cannot spray.
func TestSocketAbsentRefusalNamesAWorkingRecovery(t *testing.T) {
	touched := filepath.Join(t.TempDir(), "reached-another-tmux-command")
	pinPossibleOwnerServers(t, 4242, 4243)
	t.Cleanup(PinServerProbeForTest(4242, 4243))
	listFailureTmuxOnPath(t, touched, "error connecting to /tmp/tmux-1000/default (No such file or directory)", 1)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	err := CleanupSessions(cmd.MakeExecutor())
	if err == nil {
		t.Fatal("CleanupSessions returned nil with a live server and an absent socket")
	}
	msg := err.Error()

	if !strings.Contains(msg, "kill -USR1 4242 4243") {
		t.Errorf("error = %q, want a signal aimed at the exact server pids", msg)
	}
	if strings.Contains(msg, "pkill") {
		t.Errorf("error = %q still suggests pkill; -x cannot match `tmux: server`, and without -x "+
			"SIGUSR1 (default disposition: terminate) sprays onto anything named like tmux", msg)
	}
	for _, pid := range []string{"4242", "4243"} {
		if !strings.Contains(msg, pid) {
			t.Errorf("error = %q, want it to name server pid %s", msg, pid)
		}
	}
}

// The refusal offers `kill -USR1` only for a positively identified server
// (#7541). A candidate af could not identify, such as a darwin tmux client
// (named "tmux" exactly like a server there), is named with a check to run
// first, because SIGUSR1 terminates a client. Following the advice must never
// kill the user's own tmux client.
func TestSocketAbsentRefusalNeverOffersASignalForAnUnidentifiedCandidate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pids        []int
		identified  []int
		want        []string
		wantMissing []string
	}{
		{
			name: "one identified server beside an unidentified candidate", pids: []int{4242, 4243}, identified: []int{4242},
			want: []string{"a tmux server is running for this user (pid 4242)", "kill -USR1 4242 —",
				"pid 4243 could not be identified as a tmux server", "ps -o pid,comm,args -p 4243",
				"before signalling pid 4243: SIGUSR1 terminates a process that is not a tmux server"},
			wantMissing: []string{"kill -USR1 4242 4243", "kill -USR1 4243"},
		},
		{
			name: "only unidentified candidates", pids: []int{4243, 4244},
			want: []string{"a tmux server may be running for this user", "pids 4243, 4244 could not be identified as tmux servers",
				"ps -o pid,comm,args -p 4243,4244", "look for `tmux: server`"},
			wantMissing: []string{"kill -USR1 4243", "kill -USR1 4244"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			touched := filepath.Join(t.TempDir(), "reached-another-tmux-command")
			pinServerIdentityForTest(t, tc.identified...)
			pinSocketBindingForTest(t, func(int, string) (socketBinding, string) { return socketBindingUnresolved, "" })
			t.Cleanup(PinServerProbeForTest(tc.pids...))
			listFailureTmuxOnPath(t, touched, "error connecting to /tmp/tmux-1000/default (No such file or directory)", 1)
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

			err := CleanupSessions(cmd.MakeExecutor())
			if err == nil {
				t.Fatal("CleanupSessions returned nil with a possible owner and an absent socket")
			}
			msg := err.Error()
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error = %q, want it to contain %q", msg, w)
				}
			}
			for _, w := range tc.wantMissing {
				if strings.Contains(msg, w) {
					t.Errorf("error = %q offers %q: a candidate not identified as a tmux server must never be offered SIGUSR1", msg, w)
				}
			}
		})
	}
}

// TestSocketAbsentWithUnreadableProcessTableStillRefuses covers the branch that
// has no pid to offer: an unreadable process table is not evidence of absence,
// so the sweep must still refuse — and the advice must degrade to "go find it"
// rather than print a signal aimed at nothing.
func TestSocketAbsentWithUnreadableProcessTableStillRefuses(t *testing.T) {
	touched := filepath.Join(t.TempDir(), "reached-another-tmux-command")
	t.Cleanup(PinServerProbeForTest(0)) // the "could not read the table" sentinel
	listFailureTmuxOnPath(t, touched, "error connecting to /tmp/tmux-1000/default (No such file or directory)", 1)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	err := CleanupSessions(cmd.MakeExecutor())
	if err == nil {
		t.Fatal("an unreadable process table is not proof no server is running; the sweep must refuse")
	}
	if strings.Contains(err.Error(), "pid 0") || strings.Contains(err.Error(), "kill -USR1 0") {
		t.Errorf("error = %q leaked the no-pid sentinel into user-facing copy", err)
	}
	if !strings.Contains(err.Error(), "ps -o pid,comm,args") {
		t.Errorf("error = %q, want it to say how to FIND the server when it cannot name one", err)
	}
}

// TestOnlyServerProcessesCountAsALiveServer guards the predicate behind the
// socket-absent refusal (Codex on #2956).
//
// tmux retitles its processes, and MEASURED on Linux the server and a client are
// "tmux: server" and "tmux: client". A HasPrefix(comm, "tmux") test counts the
// CLIENT as a server — and a client exists whenever anyone is actually using
// tmux, so that mistake turns an ordinary ENOENT into "a server is running",
// refuses the reset, and aims `kill -USR1` at a process that cannot recreate a
// server socket.
//
// Measured directly, by attaching a real client to a private server:
//
//	pid=1034121 comm='tmux: server'
//	pid=1034153 comm='tmux: client'
func TestOnlyServerProcessesCountAsALiveServer(t *testing.T) {
	for _, tc := range []struct {
		comm string
		want bool
	}{
		{"tmux: server", true},
		// A server tmux started under a RENAMED argv[0]: `exec -a foo tmux`, or
		// a copy/symlink named otherwise. On Linux its kernel task name is
		// "<name>: server" (measured on tmux 3.4, #7242, and documented in
		// internal/proctree/tmuxidentity.go). Before the suffix fix this shape
		// was dropped at the prefilter, the owner never became a candidate, and
		// the socket-absent probe answered a determinate "unclaimed" while the
		// server and its panes were alive — licensing `af reset`'s destructive
		// cleanup against a still-running server (#2875). Admitting it is the
		// prefilter's own safe direction: it fails proctree.IsTmuxServer (argv[0]
		// basename != "tmux") and is remembered as an unprobed possible owner,
		// keeping the answer "claimed" so reset refuses.
		{"bx: server", true},
		{"foo: server", true},
		// The kernel keeps 15 bytes of task name and tmux 3.5a then cuts the
		// title at its last space, so a renamed server's title can end at a
		// bare colon or hold no suffix at all — shapes no suffix predicate
		// can see. Executable identity, asked in liveTmuxServerPIDs, is what
		// admits them now; the fast path alone never does.
		{"mediumtmux:", false},     // "mediumtmux: server (...)" space-cut to the bare colon
		{"mediumtmux: ser", false}, // "mediumtmux: server" cut mid-suffix
		{"verylongtmuxnam", false}, // "verylongtmuxname: server" past the suffix
		{"gnome-terminal-", false}, // an ordinary long name, not a title
		{"notmux: serverX", false}, // the suffix is strict, not a prefix
		// The fallback for builds/platforms that do not retitle. Exact only.
		{"tmux", true},
		// The one that made this a bug: a client is not a server.
		{"tmux: client", false},
		// A renamed client is still not a server: the ": server" suffix is the
		// server's title, never a client's ("<name>: client").
		{"bx: client", false},
		// Neighbours a prefix match would also have swallowed.
		{"tmuxinator", false},
		{"tmux-mem-cpu-load", false},
		{"tmuxp", false},
		{"", false},
		{"emacs", false},
	} {
		t.Run(tc.comm, func(t *testing.T) {
			if got := isTmuxServerComm(tc.comm); got != tc.want {
				t.Errorf("isTmuxServerComm(%q) = %v, want %v", tc.comm, got, tc.want)
			}
		})
	}
}

// spawnNamedSleeper runs the sleeper fixture under a copy named as given,
// keeps it alive for the test, and returns its pid. The sleeper sleeps for
// argv[1] seconds — or 300 when argv[1] is not a number — so its argv tail is
// free to be a tmux invocation, a number, or anything else.
func spawnNamedSleeper(t *testing.T, binPath string, args ...string) int {
	t.Helper()
	c := exec.Command(binPath, args...)
	if err := c.Start(); err != nil {
		t.Fatalf("start sleeper %s: %v", binPath, err)
	}
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	return c.Process.Pid
}

// #7689: candidacy comes from executable identity, not from spelling a
// title. A process running the tmux binary af execs is a candidate
// whatever its argv[0] or task name; the title fast path only ADMITS, and
// the exe read can reject only conclusively — a different inode — or keep
// the process when the read is refused. Every argv tail stays a candidate
// for a tmux-exe process: a server's argv is its client's verbatim, so no
// tail spelling can rule one out — and none is even consulted now.
func TestLiveServerPrefilterDecidesByExecutableIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the retitle spellings the prefilter works around are Linux shapes")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("the exe comparison needs the tmux binary af would run: %v", err)
	}
	dir := t.TempDir()

	// Live same-uid processes on FOREIGN executables whose comms spell the
	// shapes that used to keep them candidates — a 15-byte title lookalike,
	// a space-truncated bare-colon title, an ordinary long name. The exe
	// read now rules each out conclusively: the inode is the sleeper's, not
	// tmux's. (Filenames may carry ':' and ' ', so the comm spellings are
	// real file names.)
	titleLookalike := spawnNamedSleeper(t,
		sleeper.Named(t, filepath.Join(dir, "verylongtmuxnam")), "300")
	bareColon := spawnNamedSleeper(t,
		sleeper.Named(t, filepath.Join(dir, "mediumtmux:")), "300")
	plainLong := spawnNamedSleeper(t,
		sleeper.Named(t, filepath.Join(dir, "gnome-terminal-")), "300")
	// The title fast path still admits: a comm that spells a server title
	// is a candidate whatever its executable turns out to be — admitting
	// too much is the prefilter's own safe direction, proctree.IsTmuxServer
	// decides who is signalled.
	serverTitled := spawnNamedSleeper(t,
		sleeper.Named(t, filepath.Join(dir, "zz: server")), "300")
	// tmux's own client title is the one conclusive comm: dropped before
	// the exe read, which could only answer Same anyway — a client's exe
	// IS the tmux binary.
	clientTitled := spawnNamedSleeper(t,
		sleeper.Named(t, filepath.Join(dir, "fake: client")), "300")

	pids := liveTmuxServerPIDs()
	for _, tc := range []struct {
		name string
		pid  int
		want bool
	}{
		{"15-byte title lookalike on a foreign exe is ruled out", titleLookalike, false},
		{"space-truncated bare-colon title on a foreign exe is ruled out", bareColon, false},
		{"ordinary long name is ruled out", plainLong, false},
		{"server title admits even on a foreign exe", serverTitled, true},
		{"client title is conclusive and drops", clientTitled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsInt(pids, tc.pid); got != tc.want {
				t.Errorf("liveTmuxServerPIDs() contains %d = %v, want %v", tc.pid, got, tc.want)
			}
		})
	}
}

func containsInt(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// #7647: a socket spelling that reaches its socket through a directory that
// has itself been deleted — the post-reboot or /tmp-cleaned stale-$TMUX
// shape — stays undecidable against a same-basename server, so signalling
// cannot clear the refusal. The refusal names the action that CAN — but a
// deleted component may have been a link to where a candidate is actually
// bound (#7562's own shape), and recreating it as a plain directory would
// make the spelling resolve and wrongly rule af's own server out. So the
// advice names the check FIRST — each candidate's sessions, through its own
// bound socket, against af's session names — then both branches: a
// candidate holding af's sessions IS af's server (stop it, or restore the
// link); only when none does is recreating the directory safe.
func TestSocketAbsentRefusalNamesTheDeletedDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "tmux-1000")
	sock := filepath.Join(gone, "default")
	bound := filepath.Join(root, "tmux-real", "default")

	pinPossibleOwnerServers(t, 4242)
	t.Cleanup(PinServerProbeForTest(4242))
	// The candidate's actual bound socket, as the kernel reports it — the
	// -S spelling the check must name, never the missing one.
	prev := listenerPathsOf
	listenerPathsOf = func(int) ([]string, error) { return []string{bound}, nil }
	t.Cleanup(func() { listenerPathsOf = prev })
	listFailureTmuxOnPath(t, filepath.Join(t.TempDir(), "touched"),
		"error connecting to "+sock+" (No such file or directory)", 1)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	err = CleanupSessions(cmd.MakeExecutor())
	if err == nil {
		t.Fatal("CleanupSessions returned nil while a server may own the missing socket")
	}
	msg := err.Error()
	for _, want := range []string{
		gone,               // the deleted directory is named
		"link",             // the deleted component may have been a link
		"pid 4242",         // the candidate the check names
		"tmux -S " + bound, // through its own bound socket…
		"list-sessions",    // …list its sessions
		"`af_`",            // compared with af's session names
		"stop that server", // af's server found: stop it,
		"restore the link", // or restore the link
		"mkdir -p " + gone, // otherwise recreating it
		"safe",             // is safe
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want the refusal to contain %q", msg, want)
		}
	}
	if strings.Index(msg, "list-sessions") > strings.Index(msg, "mkdir") {
		t.Errorf("error = %q, want the check named before the recreate advice", msg)
	}
}
