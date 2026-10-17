package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// spawnWatchOrphan starts inner under `sh -c` in the background of a short-lived
// group leader that exits immediately, so the returned process is ORPHANED —
// reparented away from the test process, exactly like a watch command whose
// daemon was SIGKILLed (#5329). A child of the test process would be excluded
// by the sweep's never-touch-my-own-descendants rule and prove nothing. The
// background process inherits the leader's process group and env markers.
func spawnWatchOrphan(t *testing.T, inner string, env []string) (proctree.Process, int) {
	t.Helper()
	leader := exec.Command("sh", "-c", `sh -c '`+inner+`' >/dev/null 2>&1 </dev/null & echo $!`)
	leader.Env = env
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := leader.Output()
	if err != nil {
		t.Fatalf("spawn orphan: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse orphan pid %q: %v", out, err)
	}
	pgid := leader.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	var p proctree.Process
	waitUntil(t, 5*time.Second, "orphan to appear and be reparented", func() bool {
		got, err := proctree.Lookup(pid)
		if err != nil || got.PPID == os.Getpid() {
			return false
		}
		p = got
		return true
	})
	return p, pgid
}

// watchOrphanEnv is a minimal environment carrying the given markers; empty
// values are omitted entirely.
func watchOrphanEnv(home, gen string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "AF_TASK_ID=orphan-task"}
	if gen != "" {
		env = append(env, envWatchGeneration+"="+gen)
	}
	if home != "" {
		env = append(env, "AF_HOME="+home)
	}
	return env
}

func newOrphanSweepSupervisor(t *testing.T) (*watcherSupervisor, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	s, _ := newTestSupervisor(t, staticTasks())
	s.stopGrace = 300 * time.Millisecond
	return s, home
}

func orphanStillAlive(p proctree.Process) bool {
	same, err := proctree.SameIdentity(p)
	return err == nil && same
}

// TestSweepReapsMarkedOrphan is the #5329 repro: a watch command carrying this
// home's AF_HOME and a previous generation's AF_WATCH_GEN is stopped.
func TestSweepReapsMarkedOrphan(t *testing.T) {
	s, home := newOrphanSweepSupervisor(t)
	p, _ := spawnWatchOrphan(t, "sleep 300", watchOrphanEnv(home, "previous-generation"))

	s.sweepOrphanedWatchCommands()

	waitUntil(t, 5*time.Second, "marked orphan to be reaped", func() bool { return !orphanStillAlive(p) })
}

// TestSweepEscalatesToSIGKILL covers a watch command that ignores SIGTERM: it
// survives the grace and is then killed with the whole group.
func TestSweepEscalatesToSIGKILL(t *testing.T) {
	s, home := newOrphanSweepSupervisor(t)
	// The orphan writes ready only after its trap is installed: reparenting
	// alone does not mean it has run `trap`, and a SIGTERM that lands first
	// kills it before the grace — a race a loaded box hits (#5329).
	ready := filepath.Join(t.TempDir(), "ready")
	p, _ := spawnWatchOrphan(t, `trap "" TERM; : >`+ready+`; while :; do sleep 60; done`, watchOrphanEnv(home, "previous-generation"))
	waitUntil(t, 5*time.Second, "orphan to install its TERM trap", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})

	start := time.Now()
	s.sweepOrphanedWatchCommands()

	waitUntil(t, 5*time.Second, "TERM-ignoring orphan to be SIGKILLed", func() bool { return !orphanStillAlive(p) })
	if elapsed := time.Since(start); elapsed < s.stopGrace {
		t.Fatalf("orphan died after %s, before the %s grace: it was not the SIGKILL escalation that stopped it", elapsed, s.stopGrace)
	}
}

// TestSweepSparesUnownedProcesses pins the ownership boundary: the same
// command under another AF home, with no markers at all, or with THIS
// generation's marker must never be signaled.
func TestSweepSparesUnownedProcesses(t *testing.T) {
	s, home := newOrphanSweepSupervisor(t)
	foreignHome := t.TempDir()

	cases := map[string][]string{
		"foreign home":       watchOrphanEnv(foreignHome, "previous-generation"),
		"no markers":         watchOrphanEnv("", ""),
		"home but no gen":    watchOrphanEnv(home, ""),
		"own generation":     watchOrphanEnv(home, s.generation),
		"gen but no AF_HOME": watchOrphanEnv("", "previous-generation"),
	}
	spawned := make(map[string]proctree.Process)
	for name, env := range cases {
		p, _ := spawnWatchOrphan(t, "sleep 300", env)
		spawned[name] = p
	}

	s.sweepOrphanedWatchCommands()

	for name, p := range spawned {
		if !orphanStillAlive(p) {
			t.Errorf("%s: pid %d was signaled; the sweep must spare it", name, p.PID)
		}
	}
}

// TestSweepSkipsForeignUID uses the seams to stage what an unprivileged test
// cannot: a fully marked watch command owned by a different uid.
func TestSweepSkipsForeignUID(t *testing.T) {
	s, home := newOrphanSweepSupervisor(t)
	canonicalHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	const fakePID = 999_999_001
	self := os.Getpid()

	restore := stubWatchOrphanSeams()
	defer restore()
	watchOrphanSnapshot = func() (map[int]proctree.Process, error) {
		return map[int]proctree.Process{fakePID: {PID: fakePID, PPID: 1}}, nil
	}
	watchOrphanUID = func(pid int) (int, bool) {
		if pid == self {
			return 1000, true
		}
		return 1001, true
	}
	var environReads []int
	watchOrphanEnviron = func(pid int) ([]string, error) {
		environReads = append(environReads, pid)
		return []string{"AF_HOME=" + canonicalHome, envWatchGeneration + "=previous-generation"}, nil
	}
	watchOrphanPGID = func(pid int) (int, error) {
		if pid == self {
			return 42, nil
		}
		return fakePID, nil
	}
	var signals []string
	watchOrphanKillGroup = func(pgid int, sig syscall.Signal) error {
		signals = append(signals, strconv.Itoa(pgid)+":"+sig.String())
		return nil
	}

	s.sweepOrphanedWatchCommands()

	if len(signals) != 0 {
		t.Fatalf("foreign-uid process group was signaled: %v", signals)
	}
	if len(environReads) != 0 {
		t.Fatalf("environ of a foreign-uid process was read (%v); the uid gate must come first", environReads)
	}
}

func stubWatchOrphanSeams() func() {
	snapshot, environ, uid, pgid, kill, sleep :=
		watchOrphanSnapshot, watchOrphanEnviron, watchOrphanUID, watchOrphanPGID, watchOrphanKillGroup, watchOrphanSleep
	identity, executable := watchOrphanSameIdentity, watchOrphanExecutable
	return func() {
		watchOrphanSnapshot, watchOrphanEnviron, watchOrphanUID, watchOrphanPGID, watchOrphanKillGroup, watchOrphanSleep =
			snapshot, environ, uid, pgid, kill, sleep
		watchOrphanSameIdentity, watchOrphanExecutable = identity, executable
	}
}

// TestWatchChildEnvOverridesInheritedMarkers pins the first-match hazard: a
// daemon started inside another home's tmux pane inherits that home's AF_HOME,
// and environ readers take the FIRST match, so a stale copy left in place would
// make this daemon's own watch commands look foreign.
func TestWatchChildEnvOverridesInheritedMarkers(t *testing.T) {
	s, _ := newOrphanSweepSupervisor(t)
	t.Setenv("AF_HOME", "/stale/foreign/home")
	t.Setenv(envWatchGeneration, "stale-generation")
	t.Setenv("AF_TASK_ID", "stale-task")
	t.Setenv("AF_TASK_NAME", "stale-name")
	t.Setenv("AF_PROJECT_PATH", "/stale/project")
	t.Setenv(tmux.EnvMarkerSession, "af_unrelated_pane")
	t.Setenv(tmux.EnvMarkerGeneration, "pane-generation")
	t.Setenv("AF_TESTGUARD_RUN", "testguard-run")

	env := s.watchChildEnv("task-1", "name-1", "/project")

	wantHome, err := watchMarkerHome()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"AF_HOME":          wantHome,
		envWatchGeneration: s.generation,
		"AF_TASK_ID":       "task-1",
		"AF_TASK_NAME":     "name-1",
		"AF_PROJECT_PATH":  "/project",
	}
	for key, value := range want {
		count := 0
		for _, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%s appears %d times in the watch env, want exactly once", key, count)
		}
		if got, _ := firstEnv(env, key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	// Pane ancestry must not reach a process that is not in that pane, or the
	// tmux session orphan reaper could kill it with the unrelated session.
	for _, key := range []string{tmux.EnvMarkerSession, tmux.EnvMarkerGeneration} {
		if got, ok := firstEnv(env, key); ok {
			t.Errorf("%s = %q leaked into the watch env; inherited pane markers must be dropped", key, got)
		}
	}
	if got, _ := firstEnv(env, "AF_TESTGUARD_RUN"); got != "testguard-run" {
		t.Errorf("AF_TESTGUARD_RUN = %q, want it kept for leak attribution", got)
	}
}

// TestOrphanSweepRunsOncePerGeneration: every reconcile funnels through the
// sweep hook, but only the first may scan — later reconciles run while this
// generation's own watchers are live.
func TestOrphanSweepRunsOncePerGeneration(t *testing.T) {
	s, _ := newOrphanSweepSupervisor(t)
	s.loadTasks = func() ([]task.Task, error) { return nil, nil }
	s.reapOrphans = true

	restore := stubWatchOrphanSeams()
	defer restore()
	var mu sync.Mutex
	scans := 0
	watchOrphanSnapshot = func() (map[int]proctree.Process, error) {
		mu.Lock()
		scans++
		mu.Unlock()
		return map[int]proctree.Process{}, nil
	}

	for i := 0; i < 2; i++ {
		if err := s.Reload(); err != nil {
			t.Fatalf("Reload %d: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if scans != 1 {
		t.Fatalf("process table scanned %d times across two reloads, want exactly 1", scans)
	}
}

// TestNewWatchGenerationIsUnique guards the premise the sweep rests on: two
// generations never share a token.
func TestNewWatchGenerationIsUnique(t *testing.T) {
	a, b := newWatchGeneration(), newWatchGeneration()
	if a == "" || a == b {
		t.Fatalf("generation tokens %q and %q must be non-empty and distinct", a, b)
	}
}

// fakeOrphanProc is one row of a fabricated process table for the
// infrastructure-sparing tests: its group, and the environment it reports.
type fakeOrphanProc struct {
	proc proctree.Process
	pgid int
	env  []string
}

// sweepFakeTable runs the sweep over a fabricated, same-uid process table and
// returns the groups it SIGTERMed. Every process reports as alive until
// signaled, and every signal-0 probe answers ESRCH so the wait ends at once;
// consistently, a SIGTERMed group's members are gone from later snapshots, so
// the sweep's follow-up scans see the table a real reap leaves behind.
func sweepFakeTable(t *testing.T, home string, procs []fakeOrphanProc) []int {
	t.Helper()
	return sweepFakeTableWith(t, home, procs, nil)
}

// sweepFakeTableWith is sweepFakeTable with a hook that runs after every seam
// is stubbed and before the sweep, so a test can see the supervisor (its
// generation) or override a seam for one process.
func sweepFakeTableWith(t *testing.T, home string, procs []fakeOrphanProc, before func(s *watcherSupervisor)) []int {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", home)
	s, _ := newTestSupervisor(t, staticTasks())
	self := os.Getpid()

	restore := stubWatchOrphanSeams()
	t.Cleanup(restore)
	byPID := make(map[int]fakeOrphanProc)
	var termed []int
	watchOrphanSnapshot = func() (map[int]proctree.Process, error) {
		snap := make(map[int]proctree.Process)
		for _, p := range procs {
			byPID[p.proc.PID] = p
		}
		for _, p := range procs {
			// The CURRENT pgid, through the seam, so a test that moves a
			// process to another group sees it reaped there.
			if pgid, err := watchOrphanPGID(p.proc.PID); err == nil && slices.Contains(termed, pgid) {
				continue
			}
			snap[p.proc.PID] = p.proc
		}
		return snap, nil
	}
	watchOrphanUID = func(int) (int, bool) { return 1000, true }
	watchOrphanPGID = func(pid int) (int, error) {
		if pid == self {
			return 42, nil
		}
		if p, ok := byPID[pid]; ok {
			return p.pgid, nil
		}
		return 0, syscall.ESRCH
	}
	watchOrphanEnviron = func(pid int) ([]string, error) {
		if p, ok := byPID[pid]; ok && p.env != nil {
			return p.env, nil
		}
		return nil, proctree.ErrEnvUnreadable
	}
	watchOrphanSameIdentity = func(proctree.Process) (bool, error) { return true, nil }
	watchOrphanExecutable = func() (string, error) { return "/usr/local/bin/af", nil }
	watchOrphanSleep = func(time.Duration) {}
	watchOrphanKillGroup = func(pgid int, sig syscall.Signal) error {
		switch sig {
		case syscall.SIGTERM:
			termed = append(termed, pgid)
			return nil
		default:
			return syscall.ESRCH
		}
	}

	if before != nil {
		before(s)
	}
	s.sweepOrphanedWatchCommands()
	return termed
}

// TestSweepSparesGroupsHoldingAfInfrastructure is the #5329 review's hole: a
// watch script that runs `af` while the daemon is down ad-hoc-spawns a daemon
// whose own environ still carries the stale markers, and so does the tmux
// server and every session it starts. Those groups are af infrastructure and
// must never be signaled, while an ordinary marked group still is.
func TestSweepSparesGroupsHoldingAfInfrastructure(t *testing.T) {
	home := t.TempDir()
	canonicalHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	marked := func(extra ...string) []string {
		return append([]string{"AF_HOME=" + canonicalHome, envWatchGeneration + "=previous-generation", "AF_TASK_ID=t1"}, extra...)
	}
	const control = 900
	cases := []struct {
		name    string
		members []fakeOrphanProc
	}{
		{
			name: "tmux server member",
			members: []fakeOrphanProc{
				{proc: proctree.Process{PID: 100, PPID: 1, SID: 100, Comm: "tmux: server"}, pgid: 100, env: marked()},
			},
		},
		{
			name: "unmarked tmux server beside a marked watch command",
			members: []fakeOrphanProc{
				{proc: proctree.Process{PID: 200, PPID: 1, SID: 200, Comm: "sh"}, pgid: 200, env: marked()},
				{proc: proctree.Process{PID: 201, PPID: 200, SID: 200, Comm: "tmux: server"}, pgid: 200},
			},
		},
		{
			// darwin cannot retitle: a server there is a `tmux` comm in the
			// daemon(3) shape — its own session leader, reparented to init.
			name: "un-retitled tmux server",
			members: []fakeOrphanProc{
				{proc: proctree.Process{PID: 500, PPID: 1, SID: 500, Comm: "tmux"}, pgid: 500, env: marked()},
			},
		},
		{
			name: "af session-marked member",
			members: []fakeOrphanProc{
				{proc: proctree.Process{PID: 300, PPID: 1, SID: 300, Comm: "claude"}, pgid: 300, env: marked("AF_SESSION=af_pane")},
			},
		},
		{
			name: "af daemon leading its own session",
			members: []fakeOrphanProc{
				{proc: proctree.Process{PID: 400, PPID: 1, SID: 400, Comm: "af"}, pgid: 400, env: marked()},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			procs := append([]fakeOrphanProc{
				// The control: an ordinary marked watch command, plus a
				// transient af CLI call inside the same group. The CLI is not
				// a session leader, so it must not shield the orphan.
				{proc: proctree.Process{PID: control, PPID: 1, SID: control, Comm: "sh"}, pgid: control, env: marked()},
				{proc: proctree.Process{PID: control + 1, PPID: control, SID: control, Comm: "af"}, pgid: control, env: marked()},
			}, tc.members...)

			termed := sweepFakeTable(t, home, procs)

			if len(termed) != 1 || termed[0] != control {
				t.Fatalf("SIGTERMed groups %v, want only the control group %d", termed, control)
			}
		})
	}
}

// markedOrphanEnv returns a fake environ marking a process as a previous
// generation's watch command in home.
func markedOrphanEnv(t *testing.T, home string) []string {
	t.Helper()
	canonicalHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	return []string{"AF_HOME=" + canonicalHome, envWatchGeneration + "=previous-generation", "AF_TASK_ID=t1"}
}

// TestSweepReapsAncestorInForeignGroup is the play-test's leg-D shape (#5329):
// a watch script that runs `af` while the daemon is down is still mid-call when
// the daemon it ad-hoc-spawned sweeps, so the orphan is this daemon's ANCESTOR.
// It sits in a foreign process group (startDaemonChild Setsids the daemon), so
// it must be reaped now rather than survive a whole extra generation.
func TestSweepReapsAncestorInForeignGroup(t *testing.T) {
	home := t.TempDir()
	self := os.Getpid()
	const orphan = 500
	procs := []fakeOrphanProc{
		// The orphaned watch loop and its pending `af` call, which is this
		// daemon's parent: orphan → af CLI → self.
		{proc: proctree.Process{PID: orphan, PPID: 1, SID: 77, Comm: "sh"}, pgid: orphan, env: markedOrphanEnv(t, home)},
		{proc: proctree.Process{PID: orphan + 1, PPID: orphan, SID: 77, Comm: "af"}, pgid: orphan, env: markedOrphanEnv(t, home)},
		{proc: proctree.Process{PID: self, PPID: orphan + 1, SID: self, Comm: "af"}, pgid: 42},
	}

	termed := sweepFakeTable(t, home, procs)

	if len(termed) != 1 || termed[0] != orphan {
		t.Fatalf("SIGTERMed groups %v, want the ancestor orphan's group %d", termed, orphan)
	}
}

// TestSweepSparesOwnGroup pins what replaced the ancestor exclusion: whatever
// a process carries, a group that shares this daemon's own pgid is never
// signaled, while an ordinary marked group in the same table still is.
func TestSweepSparesOwnGroup(t *testing.T) {
	home := t.TempDir()
	const ownGroupMember, control = 600, 900
	procs := []fakeOrphanProc{
		// sweepFakeTable reports pgid 42 for this daemon.
		{proc: proctree.Process{PID: ownGroupMember, PPID: 1, SID: 77, Comm: "sh"}, pgid: 42, env: markedOrphanEnv(t, home)},
		{proc: proctree.Process{PID: control, PPID: 1, SID: control, Comm: "sh"}, pgid: control, env: markedOrphanEnv(t, home)},
	}

	termed := sweepFakeTable(t, home, procs)

	if len(termed) != 1 || termed[0] != control {
		t.Fatalf("SIGTERMed groups %v, want only the control group %d (never the daemon's own group 42)", termed, control)
	}
}
