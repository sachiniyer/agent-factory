package daemon

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
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
	p, _ := spawnWatchOrphan(t, `trap "" TERM; while :; do sleep 60; done`, watchOrphanEnv(home, "previous-generation"))

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
	return func() {
		watchOrphanSnapshot, watchOrphanEnviron, watchOrphanUID, watchOrphanPGID, watchOrphanKillGroup, watchOrphanSleep =
			snapshot, environ, uid, pgid, kill, sleep
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
