package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

type harness struct {
	t    *testing.T
	bin  string
	home string
	repo string
}

type instanceData struct {
	Title       string `json:"title"`
	TmuxName    string `json:"tmux_name"`
	BackendType string `json:"backend_type"`
	Tabs        []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind int    `json:"kind"`
	} `json:"tabs"`
	Worktree struct {
		WorktreePath string `json:"worktree_path"`
	} `json:"worktree"`
}

func TestBlackBoxCLIDaemonLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive daemon integration test; skipped under -short — see #2052")
	}
	h := newHarness(t)

	created := h.createSession("alpha")
	if created.Title != "alpha" {
		t.Fatalf("unexpected create response: %+v", created)
	}

	socketPath := filepath.Join(h.home, "daemon.sock")
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("daemon socket was not created: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("daemon socket permissions = %o, want 0600", info.Mode().Perm())
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("daemon socket path is not a socket: mode=%s", info.Mode())
	}
	if !pidAlive(readDaemonPID(t, h.home)) {
		t.Fatalf("daemon pid is not alive")
	}

	assertTitles(t, h.listSessions(), "alpha")

	h.run("sessions", "send-prompt", "alpha", "hello-integration")
	waitUntil(t, 5*time.Second, "preview contains sent prompt", func() bool {
		return strings.Contains(h.preview("alpha"), "hello-integration")
	})

	h.run("sessions", "kill", "alpha")
	waitUntil(t, 5*time.Second, "session removed from CLI list", func() bool {
		return !hasTitle(h.listSessions(), "alpha")
	})
	if created.TmuxName != "" && tmuxSessionExists(created.TmuxName) {
		t.Fatalf("tmux session %q still exists after kill", created.TmuxName)
	}
	if wt := created.Worktree.WorktreePath; wt != "" {
		if _, err := os.Stat(wt); !os.IsNotExist(err) {
			t.Fatalf("worktree %q still exists after kill; stat err=%v", wt, err)
		}
	}
}

func TestConcurrentCLIClientsUseDaemonCoordinator(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive daemon integration test; skipped under -short — see #2052")
	}
	h := newHarness(t)

	// Warm the daemon before stressing concurrent lifecycle operations. This
	// keeps this test focused on daemon-owned mutation coordination rather than
	// daemon startup races.
	h.createSession("warmup")

	var wg sync.WaitGroup
	distinctErrs := make(chan error, 5)
	duplicateErrs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("parallel-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.runResult("sessions", "--repo", h.repo, "create", "--name", name, "--program", tmux.ProgramClaude)
			distinctErrs <- err
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.runResult("sessions", "--repo", h.repo, "create", "--name", "dupe", "--program", tmux.ProgramClaude)
			duplicateErrs <- err
		}()
	}
	wg.Wait()
	close(distinctErrs)
	close(duplicateErrs)

	for err := range distinctErrs {
		if err != nil {
			t.Fatalf("distinct concurrent create failed: %v", err)
		}
	}

	duplicateSuccesses := 0
	for err := range duplicateErrs {
		if err == nil {
			duplicateSuccesses++
		} else if !strings.Contains(err.Error(), "already exists") && !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("unexpected duplicate create error: %v", err)
		}
	}
	if duplicateSuccesses != 1 {
		t.Fatalf("duplicate create successes = %d, want 1", duplicateSuccesses)
	}

	sessions := h.listSessions()
	for i := 0; i < 5; i++ {
		if !hasTitle(sessions, fmt.Sprintf("parallel-%d", i)) {
			t.Fatalf("missing parallel-%d in sessions: %+v", i, sessions)
		}
	}
	if got := countTitle(sessions, "dupe"); got != 1 {
		t.Fatalf("duplicate title records = %d, want 1; sessions=%+v", got, sessions)
	}
}

func TestScheduledTaskRunnerUsesDaemonAndAllocatesRerunTitle(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive daemon integration test; skipped under -short — see #2052")
	}
	h := newHarness(t)
	writeTasksFile(t, h.home, []map[string]interface{}{
		{
			"id":           "task-one",
			"name":         "nightly",
			"prompt":       "",
			"cron_expr":    manualTriggerFixtureCron(time.Now()),
			"project_path": h.repo,
			"program":      tmux.ProgramClaude,
			"enabled":      true,
			"created_at":   time.Now().Format(time.RFC3339Nano),
		},
	})

	// `af tasks trigger` rides the same daemon.RunTask path the in-daemon
	// cron scheduler fires (#782), so this also covers scheduled runs.
	h.run("tasks", "trigger", "task-one")
	h.run("tasks", "trigger", "task-one")

	sessions := h.listSessions()
	assertTitles(t, sessions, "nightly", "nightly-2")

	var tasksFile struct {
		SchemaVersion int `json:"schema_version"`
		Tasks         []struct {
			ID            string     `json:"id"`
			LastRunAt     *time.Time `json:"last_run_at"`
			LastRunStatus string     `json:"last_run_status"`
		} `json:"tasks"`
	}
	readJSONFile(t, filepath.Join(h.home, "tasks.json"), &tasksFile)
	tasks := tasksFile.Tasks
	if len(tasks) != 1 || tasks[0].LastRunAt == nil || tasks[0].LastRunStatus != "started" {
		t.Fatalf("task status was not updated after run: %+v", tasks)
	}
}

// manualTriggerFixtureCron keeps the task enabled and schedulable while placing
// its next automatic fire outside this test's manual-trigger window. An
// every-minute fixture lets robfig/cron race h.run below for the same nonblocking
// task lock, so either manual invocation can fail without exercising title
// allocation at all (#2163).
func manualTriggerFixtureCron(now time.Time) string {
	future := now.Add(24 * time.Hour)
	return fmt.Sprintf("%d %d %d %d *", future.Minute(), future.Hour(), future.Day(), future.Month())
}

func TestScheduledTaskManualTriggerFixtureCannotRaceTheScheduler(t *testing.T) {
	now := time.Date(2026, time.July, 21, 20, 33, 42, 0, time.UTC)
	schedule, err := task.ParseCron(manualTriggerFixtureCron(now))
	if err != nil {
		t.Fatalf("parse fixture cron: %v", err)
	}
	if next := schedule.Next(now); next.Before(now.Add(12 * time.Hour)) {
		t.Fatalf("fixture can fire during manual triggers: next run is %s after %s", next, now)
	}
}

func TestDaemonLifecycleRecoversFromStaleSocketAndDeadDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive daemon integration test; skipped under -short — see #2052")
	}
	h := newHarness(t)

	staleSocket := filepath.Join(h.home, "daemon.sock")
	if err := os.WriteFile(staleSocket, []byte("not a socket"), 0600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}
	h.createSession("after-stale-socket")
	firstPID := readDaemonPID(t, h.home)

	if err := os.Remove(staleSocket); err != nil {
		t.Fatalf("remove live socket: %v", err)
	}
	h.createSession("after-missing-socket")
	secondPID := readDaemonPID(t, h.home)
	if secondPID == firstPID {
		t.Fatalf("daemon PID did not change after live socket was removed; pid=%d", secondPID)
	}

	killPID(secondPID)
	waitUntil(t, 5*time.Second, "daemon process exits", func() bool {
		return !pidAlive(secondPID)
	})
	h.createSession("after-dead-daemon")
	thirdPID := readDaemonPID(t, h.home)
	if thirdPID == secondPID {
		t.Fatalf("daemon PID did not change after daemon was killed; pid=%d", thirdPID)
	}

	assertTitles(t, h.listSessions(), "after-stale-socket", "after-missing-socket", "after-dead-daemon")
}

// #1592 Phase 4 PR7: TestRemoteHookImportKillAndFailureModes was removed with the
// remote-hook enumeration/import model (list_cmd + ImportRemoteHookSessions +
// delete-by-name). The migrated hook backend is provision-and-expose: its full
// lifecycle (launch_cmd → drive over http/ws → delete_cmd teardown) is proven by
// TestRemoteHookRoundTripMockRemote against a real af agent-server.

func newHarness(t *testing.T) *harness {
	t.Helper()
	requireTool(t, "git")
	requireTool(t, "tmux")

	// #1056: run every tmux touch — the parent's has-session/kill-session
	// probes, the exec'd af CLI, and the daemon it spawns (all inherit the
	// environment) — against a private tmux server that is killed when the
	// test ends, so no af_ session can leak onto the developer's server even
	// when per-session cleanup below fails.
	testguard.IsolateTmux(t)

	// SocketTempDir, not t.TempDir: the daemon derives daemon.sock from this home,
	// and t.TempDir's test-name-bearing path overruns sun_path on macOS (#1940).
	home := testguard.SocketTempDir(t)
	// t.Setenv (not os.Setenv) so the variable is restored when the test
	// ends; an unrestored value leaks the previous test's (deleted) tempdir
	// into every later test in the package (#837 audit).
	t.Setenv("AGENT_FACTORY_HOME", home)
	// The enum-only program model (#658) routes session.Program through
	// session.injectSystemPrompt, which appends agent-specific flags
	// (e.g. --plugin-dir for claude) to the resolved command. Plain `cat`
	// errors out on those flags, so write a shell wrapper that swallows any
	// args and behaves as a stdin-reading pane process. ProgramOverrides
	// in testConfig points the claude enum at this wrapper.
	//
	// The wrapper prints a "❯" ready prompt before reading stdin so the
	// daemon's waitForReady loop recognises the pane as ready. The create
	// path now always waits for readiness — even for empty-prompt sessions
	// (#698) — so a real agent's startup prompt must be emulated here.
	wrapper := filepath.Join(home, "fake-agent.sh")
	writeFile(t, wrapper, "#!/bin/sh\nprintf '❯ '\nexec cat\n", 0755)
	writeConfigWithProgramPath(t, home, wrapper)

	h := &harness{
		t:    t,
		bin:  buildBinary(t),
		home: home,
		repo: setupGitRepo(t),
	}
	t.Cleanup(func() {
		h.cleanupSessions()
		killDaemonFromHome(home)
	})
	return h
}

func buildBinary(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(file))
	bin := filepath.Join(t.TempDir(), "af")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, repoRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	return bin
}

func setupGitRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	runExternal(t, "", "git", "init", repo)
	runExternal(t, repo, "git", "config", "user.email", "test@example.com")
	runExternal(t, repo, "git", "config", "user.name", "Test User")
	runExternal(t, repo, "git", "commit", "--allow-empty", "-m", "init")
	return repo
}

func (h *harness) createSession(name string) instanceData {
	out := h.run("sessions", "--repo", h.repo, "create", "--name", name, "--program", tmux.ProgramClaude)
	var data instanceData
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		h.t.Fatalf("parse create response: %v\n%s", err, out)
	}
	return data
}

func (h *harness) listSessions() []instanceData {
	out := h.run("sessions", "--repo", h.repo, "list")
	var data []instanceData
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		h.t.Fatalf("parse list response: %v\n%s", err, out)
	}
	return data
}

func (h *harness) preview(title string) string {
	out := h.run("sessions", "preview", title)
	var data map[string]string
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		h.t.Fatalf("parse preview response: %v\n%s", err, out)
	}
	return data["content"]
}

func (h *harness) run(args ...string) string {
	h.t.Helper()
	out, err := h.runResult(args...)
	if err != nil {
		h.t.Fatalf("af %s failed: %v", strings.Join(args, " "), err)
	}
	return out
}

func (h *harness) runResult(args ...string) (string, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, args...)
	cmd.Dir = h.repo
	cmd.Env = append(os.Environ(),
		"AGENT_FACTORY_HOME="+h.home,
		"TERM=xterm",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), fmt.Errorf("timed out; stderr=%s", stderr.String())
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("%w; stderr=%s stdout=%s", err, stderr.String(), stdout.String())
	}
	return stdout.String(), nil
}

func (h *harness) cleanupSessions() {
	out, err := h.runResult("sessions", "--repo", h.repo, "list")
	if err != nil {
		return
	}
	var sessions []instanceData
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		return
	}
	for _, item := range sessions {
		_, _ = h.runResult("sessions", "kill", item.Title)
		if item.TmuxName != "" && tmuxSessionExists(item.TmuxName) {
			_ = exec.Command("tmux", "kill-session", fmt.Sprintf("-t=%s", item.TmuxName)).Run()
		}
	}
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not available: %v", name, err)
	}
}

func runExternal(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// writeConfigWithProgramPath persists the integration-test config with the
// claude enum routed to a path-on-disk wrapper script (see newHarness).
func writeConfigWithProgramPath(t *testing.T, home, programPath string) {
	t.Helper()
	cfg := testConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: programPath}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	writeFile(t, filepath.Join(home, config.ConfigFileName), string(raw), 0644)
}

func testConfig() *config.Config {
	return &config.Config{
		// DefaultProgram is restricted to a SupportedPrograms enum (#658).
		// Integration tests pick "claude" and route it through
		// ProgramOverrides (see writeConfigWithProgramPath) to a harmless
		// wrapper script that doesn't need a real agent binary.
		DefaultProgram:     tmux.ProgramClaude,
		DaemonPollInterval: 100,
		BranchPrefix:       "test/",
		DetachKeys:         "ctrl-w",
	}
}

func writeTasksFile(t *testing.T, home string, tasks []map[string]interface{}) {
	t.Helper()
	raw, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		t.Fatalf("marshal tasks: %v", err)
	}
	writeFile(t, filepath.Join(home, "tasks.json"), string(raw), 0644)
}

func writeScript(t *testing.T, path, body string) string {
	t.Helper()
	writeFile(t, path, "#!/bin/sh\nset -eu\n"+body+"\n", 0755)
	return path
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readJSONFile(t *testing.T, path string, dst interface{}) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("unmarshal %s: %v\n%s", path, err, raw)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", desc)
}

func assertTitles(t *testing.T, sessions []instanceData, titles ...string) {
	t.Helper()
	for _, title := range titles {
		if !hasTitle(sessions, title) {
			t.Fatalf("missing title %q in sessions: %+v", title, sessions)
		}
	}
}

func hasTitle(sessions []instanceData, title string) bool {
	return countTitle(sessions, title) > 0
}

func countTitle(sessions []instanceData, title string) int {
	count := 0
	for _, item := range sessions {
		if item.Title == title {
			count++
		}
	}
	return count
}

func tmuxSessionExists(name string) bool {
	return exec.Command("tmux", "has-session", fmt.Sprintf("-t=%s", name)).Run() == nil
}

func readDaemonPID(t *testing.T, home string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "daemon.pid"))
	if err != nil {
		t.Fatalf("read daemon.pid: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(raw), "%d", &pid); err != nil {
		t.Fatalf("parse daemon.pid %q: %v", string(raw), err)
	}
	return pid
}

func killDaemonFromHome(home string) {
	raw, err := os.ReadFile(filepath.Join(home, "daemon.pid"))
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(string(raw), "%d", &pid); err != nil {
		return
	}
	killPID(pid)
}

func killPID(pid int) {
	if pid <= 1 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err == nil {
		_ = proc.Kill()
	}
}

func pidAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
