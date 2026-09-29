package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// tmuxNamedProcess starts a real process that looks exactly like a tmux client
// to everything proctree reads: its argv[0] is a path ending in `tmux`, and,
// because the executable file itself is called `tmux`, so is its kernel task
// name — which is what every tmux process is called on darwin, where tmux
// cannot retitle itself. It is a copy of sleep, so nothing here drives tmux.
//
// A real process rather than a fixture, because the defect this guards was in
// what IsTmuxServer concluded from a live process's own table entry (#4678).
func tmuxNamedProcess(t *testing.T, dir string, attr *syscall.SysProcAttr) int {
	t.Helper()
	sleepPath, err := exec.LookPath("sleep")
	require.NoError(t, err)
	body, err := os.ReadFile(sleepPath)
	require.NoError(t, err)
	fake := filepath.Join(t.TempDir(), "tmux")
	require.NoError(t, os.WriteFile(fake, body, 0o755))

	cmd := exec.Command(fake, "300")
	cmd.Dir = dir
	cmd.SysProcAttr = attr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	pid := cmd.Process.Pid
	// Both reads the predicates make must be answerable before asserting on
	// them, or a false below could mean "unreadable" instead of "not a server".
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, lookupErr := Lookup(pid)
		if lookupErr == nil && p.Comm == "tmux" && len(Argv(pid)) > 0 {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	p, lookupErr := Lookup(pid)
	t.Fatalf("the tmux-named fixture never became readable as `tmux`: comm=%q err=%v argv=%q",
		p.Comm, lookupErr, Argv(pid))
	return 0
}

// The #4678 regression. The socket-absent probe sends SIGUSR1 — default
// disposition TERMINATE — to every pid IsTmuxServer accepts, and IsTmuxServer
// accepted any process whose argv[0] was `tmux`: every tmux client. On a macOS
// runner that killed af's own short-lived `show-options` and `kill-session`
// clients mid-startup.
//
// Both shapes af launches clients in are covered: a plain child (af's bounded
// tmux commands; not a session leader) and a session leader whose parent is
// alive (a client started on a PTY, which calls setsid).
func TestIsTmuxServer_RefusesATmuxClient(t *testing.T) {
	plain := tmuxNamedProcess(t, t.TempDir(), nil)
	require.False(t, IsTmuxServer(plain),
		"a process named tmux that is not a server must never be chosen for SIGUSR1")

	leader := tmuxNamedProcess(t, t.TempDir(), &syscall.SysProcAttr{Setsid: true})
	p, err := Lookup(leader)
	require.NoError(t, err)
	require.Equal(t, leader, p.SID, "fixture: Setsid must make it a session leader")
	require.False(t, IsTmuxServer(leader),
		"a session-leading client whose parent is alive is not a daemonised server")
}

// The leave-alone predicate still covers the client: the occupancy gate and the
// worktree reaper exclude every tmux process, and #4678 must not change that.
func TestIsTmuxProcess_CoversClientsAndRetitledArgv(t *testing.T) {
	require.True(t, IsTmuxProcess(tmuxNamedProcess(t, t.TempDir(), nil)))

	retitled := exec.Command("sleep", "300")
	retitled.Args[0] = "tmux: server (/tmp/tmux-1000/default)"
	require.NoError(t, retitled.Start())
	t.Cleanup(func() {
		_ = retitled.Process.Kill()
		_, _ = retitled.Process.Wait()
	})
	require.Eventually(t, func() bool { return len(Argv(retitled.Process.Pid)) > 0 },
		5*time.Second, 10*time.Millisecond)
	require.True(t, IsTmuxProcess(retitled.Process.Pid),
		"a setproctitle(3)-style title is tmux; matched on the whole argv[0], since Base would cut at the socket path")
	require.False(t, IsTmuxServer(retitled.Process.Pid),
		"the signalling predicate never trusts a title written into argv")

	plain := exec.Command("sleep", "300")
	require.NoError(t, plain.Start())
	t.Cleanup(func() {
		_ = plain.Process.Kill()
		_, _ = plain.Process.Wait()
	})
	require.False(t, IsTmuxProcess(plain.Process.Pid))
	require.False(t, IsTmuxProcess(-1), "unreadable reports false")
}

// A tmux client parked in a worktree is still not an occupant. af's own clients
// inherit the daemon's cwd, which can sit in a worktree; counting them would
// refuse a teardown whenever the daemon was mid-command.
func TestOccupantsOfDir_ExcludesATmuxClient(t *testing.T) {
	dir := t.TempDir()
	client := tmuxNamedProcess(t, dir, nil)
	require.Eventually(t, func() bool { _, ok := WorkingDir(client); return ok },
		5*time.Second, 10*time.Millisecond)
	ordinary := parkedProcessIn(t, dir)

	occupants, err := OccupantsOfDir(dir)
	require.NoError(t, err)
	got := pidsOf(occupants)
	require.True(t, got[ordinary], "control: an ordinary process in the same dir is reported")
	require.False(t, got[client], "a tmux client is not an occupant")
}

// Every shape IsTmuxServer has to decide, pinned without a live tmux. Each row
// names the platform fact it stands for.
func TestTmuxServerIdentity(t *testing.T) {
	const af, systemdUser = 4100, 1426
	for _, tc := range []struct {
		name string
		p    Process
		want bool
	}{
		// Linux: tmux retitles the task in proc_start, so the name is the answer.
		{"linux daemonised server under a subreaper", Process{PID: 500, PPID: systemdUser, SID: 500, Comm: "tmux: server"}, true},
		{"linux foreground -D server", Process{PID: 501, PPID: af, SID: 77, Comm: "tmux: server"}, true},
		{"linux client", Process{PID: 502, PPID: af, SID: 77, Comm: "tmux: client"}, false},
		{"linux orphaned PTY client reparented to init", Process{PID: 503, PPID: 1, SID: 503, Comm: "tmux: client"}, false},
		{"linux process not yet retitled, under a subreaper", Process{PID: 504, PPID: systemdUser, SID: 504, Comm: "tmux"}, false},

		// darwin: tmux cannot retitle, so every tmux process is "tmux" and the
		// daemon(3) shape is the only positive answer.
		{"darwin daemonised server", Process{PID: 600, PPID: 1, SID: 600, Comm: "tmux"}, true},
		{"darwin client af runs as a plain child", Process{PID: 601, PPID: af, SID: 77, Comm: "tmux"}, false},
		{"darwin client af runs on a PTY (setsid)", Process{PID: 602, PPID: af, SID: 602, Comm: "tmux"}, false},
		{"darwin client started from a shell", Process{PID: 603, PPID: 300, SID: 300, Comm: "tmux"}, false},
		{"darwin session id unreadable", Process{PID: 604, PPID: 1, SID: sidUnknown, Comm: "tmux"}, false},

		// Anything else is not positively a server.
		{"unknown title", Process{PID: 700, PPID: 1, SID: 700, Comm: "tmux: frobnicate"}, false},
		{"not tmux at all", Process{PID: 701, PPID: 1, SID: 701, Comm: "sleep"}, false},
		{"no pid", Process{PID: 0, PPID: 1, SID: 0, Comm: "tmux"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tmuxServerIdentity(tc.p))
		})
	}
}
