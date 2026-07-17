package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// Every test here is hermetic by construction (#1104 hard rules): the AF
// home is a temp dir (TestMain sandbox + per-test overrides), tmux runs on a
// private server (testguard.IsolateTmux), the /proc snapshot handed to Run
// is filtered to processes this test spawned, and every spawned process is
// killed on cleanup. Nothing here can observe — let alone signal — the real
// daemon, the real ~/.agent-factory, or the developer's tmux server.

func TestMain(m *testing.M) {
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: fail loudly if a test leaks an af_ session onto the ambient tmux
	// server (doctor tests drive real tmux via IsolateTmux).
	verifyTmux := testguard.TmuxTripwire()
	restoreHome := testguard.SandboxHome()
	// #1122: default the whole package onto a private tmux server so a test
	// that forgets IsolateTmux can never create or sweep sessions on the
	// developer's real server.
	restoreTmux := testguard.SandboxTmux()
	code := m.Run()
	restoreTmux()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := verifyTmux(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// testOptions returns Options scoped entirely to this test: temp home, temp
// scan root, fast kill windows, and a snapshot containing only pids.
func testOptions(t *testing.T, fix bool, pids ...int) Options {
	t.Helper()
	return testOptionsWithHome(t, t.TempDir(), fix, pids...)
}

// testOptionsWithHome is testOptions with a caller-chosen home, for tests
// whose spawned processes must carry an AF_HOME marker that matches (or
// deliberately mismatches) the run's ConfigDir.
func testOptionsWithHome(t *testing.T, home string, fix bool, pids ...int) Options {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", home)
	return Options{
		Fix:            fix,
		ConfigDir:      home,
		TempDir:        t.TempDir(),
		Exec:           cmd.MakeExecutor(),
		MinTempHomeAge: time.Hour,
		killGrace:      100 * time.Millisecond,
		killTermWait:   200 * time.Millisecond,
		snapshot:       snapshotOf(t, pids...),
		// Default to "no remote configured" so the non-remote suite stays
		// hermetic (no git shell-out, no reading the real repo's in-repo
		// config). The remote tests below inject their own resolver.
		remoteConfig: func() (*config.RemoteHooks, string, error) { return nil, "", nil },
		// The skew checks (#1044) all reach for real machine state, so they
		// are defaulted here to inert fakes: "nothing answered, no unit
		// installed, no af binaries to compare". Without this the suite would
		// ping whatever daemon is live on the developer's box and execute the
		// real `af` binaries found on PATH. The skew tests inject their own.
		// Mirrors what real Health() reports for a home with no daemon: nothing
		// answers the control socket, and nothing is listening on the HTTP one
		// either. HTTPSocketExists tracks the real file so a test that stages a
		// socket gets a truthful probe rather than an assumed one.
		daemonHealth: func() daemon.HealthStatus {
			httpSock := filepath.Join(home, "daemon-http.sock")
			_, statErr := os.Stat(httpSock)
			return daemon.HealthStatus{
				PingErr:          errNoDaemon,
				HTTPSocketPath:   httpSock,
				HTTPSocketExists: statErr == nil,
				HTTPDialErr:      errNoDaemon,
			}
		},
		autostartUnit:        func() daemon.AutostartUnitInfo { return daemon.AutostartUnitInfo{Supported: true} },
		autostartSupervision: func() daemon.SupervisionInfo { return daemon.SupervisionInfo{Supported: true} },
		// No autostart unit for this home by default, matching the two fakes
		// above. Tests that install one declare it with ourAutostartUnit.
		autostartServesHome: func(string) (bool, bool, error) { return false, false, nil },
		selfBinary:          func() (string, error) { return filepath.Join(home, "bin", "af"), nil },
		binaryCandidates:    func() []string { return nil },
		binaryVersion:       func(string) (string, error) { return "", errNoDaemon },
	}
}

// errNoDaemon stands in for "nothing answered" in tests that must never touch
// a real daemon.
var errNoDaemon = errors.New("no daemon (test fake)")

// snapshotOf builds a snapshot function restricted to the given pids (read
// from the real /proc), so Run can never act outside the test's processes.
func snapshotOf(t *testing.T, pids ...int) func() (map[int]proctree.Process, error) {
	t.Helper()
	return func() (map[int]proctree.Process, error) {
		full, err := proctree.Snapshot()
		if err != nil {
			return nil, err
		}
		out := map[int]proctree.Process{}
		for _, pid := range pids {
			if p, ok := full[pid]; ok {
				out[pid] = p
			}
		}
		return out, nil
	}
}

// spawnWithEnv starts a long-lived child owned by this test, with argv0 and
// args controlling how its cmdline reads and env appended to the test's
// environment. The child is a shell blocked on its `read` builtin against a
// pipe this test holds open — a single process with NO descendants, so the
// cleanup Kill can never orphan anything (a forked `sleep` here once leaked
// the very orphans this suite hunts). Waits until the child's /proc environ
// is readable (the fork→exec window would otherwise race the scan).
// It reads the child's environ through proctree, so it — and therefore every
// test built on it — cannot run without /proc. Guarding here rather than at each
// caller keeps the reason in ONE place: see #1939 (REAL DEFECT — proctree has no
// darwin backend, so doctor's process mapping is silently broken on macOS, which
// is exactly what these tests exist to prove works).
func spawnWithEnv(t *testing.T, argv0 string, extraArgs []string, env map[string]string) proctree.Process {
	testguard.RequireProcFS(t)
	t.Helper()
	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	args := append([]string{argv0, "-c", "read line"}, extraArgs...)
	c := &exec.Cmd{Path: "/bin/sh", Args: args, Env: os.Environ(), Stdin: stdinR}
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	require.NoError(t, c.Start())
	_ = stdinR.Close() // child holds its own copy
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_ = stdinW.Close()
		_, _ = c.Process.Wait()
	})
	// Reap promptly when a doctor --fix kills the child mid-test so the pid
	// doesn't linger as a zombie in later snapshots (zombies still answer
	// signal 0).
	go func() { _, _ = c.Process.Wait() }()

	require.Eventually(t, func() bool {
		_, ok := proctree.EnvValue(c.Process.Pid, "PATH")
		if len(env) > 0 {
			for k := range env {
				_, ok2 := proctree.EnvValue(c.Process.Pid, k)
				return ok2
			}
		}
		return ok
	}, 5*time.Second, 10*time.Millisecond, "child environ never became readable")

	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	p, ok := snap[c.Process.Pid]
	require.True(t, ok, "spawned child %d missing from snapshot", c.Process.Pid)
	require.Zero(t, len(proctree.TreeOf(snap, c.Process.Pid))-1,
		"test child must have no descendants — cleanup would orphan them")
	return p
}

func alive(p proctree.Process) bool { return proctree.AliveSame(p) }

func findByCheck(r *Report, check string) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func findCheckRows(r *Report, name string) []CheckResult {
	var out []CheckResult
	for _, c := range r.Checks {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// TestOrphanedProcessDetectedAndFixed is the doctor half of the #1104
// regression: a process whose AF_SESSION marker names a dead session is a
// verified orphan — reported without --fix, killed with it.
func TestOrphanedProcessDetectedAndFixed(t *testing.T) {
	testguard.RequireProcFS(t)
	testguard.IsolateTmux(t) // private server: the marked session is dead by construction

	// The orphan's AF_HOME must match the run's ConfigDir — a kill requires
	// a proven home match, not just a dead-looking session.
	home := t.TempDir()
	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		"AF_SESSION": "af_doctor-dead-session",
		"AF_HOME":    home,
	})

	// Read-only pass: reported, not touched.
	report, err := Run(testOptionsWithHome(t, home, false, orphan.PID))
	require.NoError(t, err)
	findings := findByCheck(report, "orphaned-process")
	require.Len(t, findings, 1)
	require.Contains(t, findings[0].Detail, "af_doctor-dead-session")
	require.NotEmpty(t, findings[0].FixAction)
	require.False(t, findings[0].Fixed)
	require.True(t, alive(orphan), "read-only doctor run must not signal anything")

	// --fix pass: killed, with the outcome recorded.
	report, err = Run(testOptionsWithHome(t, home, true, orphan.PID))
	require.NoError(t, err)
	findings = findByCheck(report, "orphaned-process")
	require.Len(t, findings, 1)
	require.True(t, findings[0].Fixed, "fix outcome: %v", findings[0].FixErr)
	require.Eventually(t, func() bool { return !alive(orphan) }, 5*time.Second, 25*time.Millisecond,
		"verified orphan must be dead after --fix")
	require.Zero(t, report.UnresolvedCount())
}

// TestOrphanWithoutProvenHomeSurvivesFix pins the home-match gate: a process
// marking a dead session is killed ONLY when its AF_HOME matches the active
// home. A foreign home (e.g. a concurrent play-test sandbox whose sessions
// live on a private `tmux -L` server and are invisible here) and a missing
// home marker (pre-marker spawn, unreadable environ) are both report-only.
func TestOrphanWithoutProvenHomeSurvivesFix(t *testing.T) {
	testguard.RequireProcFS(t)
	testguard.IsolateTmux(t)

	foreign := spawnWithEnv(t, "sh", nil, map[string]string{
		"AF_SESSION": "af_doctor-foreign-home",
		"AF_HOME":    t.TempDir(), // some other install's home
	})
	unmarked := spawnWithEnv(t, "sh", nil, map[string]string{
		"AF_SESSION": "af_doctor-no-home",
	})

	report, err := Run(testOptions(t, true, foreign.PID, unmarked.PID))
	require.NoError(t, err)
	findings := findByCheck(report, "orphaned-process")
	require.Len(t, findings, 2)
	for _, f := range findings {
		require.Empty(t, f.FixAction, "without a proven home match the finding must be report-only: %s", f.Detail)
		require.False(t, f.Fixed)
	}
	var foreignDetail, unmarkedDetail bool
	for _, f := range findings {
		if strings.Contains(f.Detail, "another agent-factory home") {
			foreignDetail = true
		}
		if strings.Contains(f.Detail, "no readable home marker") {
			unmarkedDetail = true
		}
	}
	require.True(t, foreignDetail, "foreign-home orphan must say whose it is")
	require.True(t, unmarkedDetail, "unmarked orphan must say why it is not killed")

	require.True(t, alive(foreign), "a foreign home's process must survive --fix")
	require.True(t, alive(unmarked), "an unproven orphan must survive --fix")
}

// TestMarkedProcessOfLiveSessionIsNeverKilled: a marker pointing at a LIVE
// session means the process escaped its pane but the session still owns it —
// report-only even under --fix.
func TestMarkedProcessOfLiveSessionIsNeverKilled(t *testing.T) {
	testguard.RequireProcFS(t)
	testguard.IsolateTmux(t)

	const name = "af_doctor-live-session"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	escapee := spawnWithEnv(t, "sh", nil, map[string]string{"AF_SESSION": name})

	report, err := Run(testOptions(t, true, escapee.PID))
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "orphaned-process"))
	escaped := findByCheck(report, "escaped-process")
	require.Len(t, escaped, 1)
	require.Empty(t, escaped[0].FixAction, "escaped processes of live sessions are report-only")
	require.True(t, alive(escapee), "process of a live session must never be killed")
}

// TestLeakedTmuxSessionReportedNotKilled: an af_ session with no backing
// record is reported with a suggested command, and --fix must NOT kill it
// (it may belong to another agent-factory home).
func TestLeakedTmuxSessionReportedNotKilled(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-leaked"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	report, err := Run(testOptions(t, true))
	require.NoError(t, err)
	leaked := findByCheck(report, "leaked-tmux-session")
	require.Len(t, leaked, 1)
	require.Contains(t, leaked[0].Detail, name)
	require.Empty(t, leaked[0].FixAction)
	require.NoError(t, exec.Command("tmux", "has-session", "-t", "="+name+":").Run(),
		"leaked session must survive --fix")
}

// TestStaleTempHomeRemovedWithFix: an abandoned AF home under the temp root
// is detected by its structural markers and removed by --fix; a fresh or
// referenced home is spared.
func TestStaleTempHomeRemovedWithFix(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, true)

	old := time.Now().Add(-48 * time.Hour)
	makeHome := func(name string) string {
		dir := filepath.Join(opts.TempDir, name)
		require.NoError(t, os.MkdirAll(dir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0644))
		for _, p := range []string{filepath.Join(dir, "config.json"), filepath.Join(dir, "state.json"), dir} {
			require.NoError(t, os.Chtimes(p, old, old))
		}
		return dir
	}
	stale := makeHome("tmp.stale-home")
	fresh := filepath.Join(opts.TempDir, "tmp.fresh-home")
	require.NoError(t, os.MkdirAll(fresh, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(fresh, "config.json"), []byte("{}"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(fresh, "state.json"), []byte("{}"), 0644))
	notAHome := filepath.Join(opts.TempDir, "random-dir")
	require.NoError(t, os.MkdirAll(notAHome, 0755))
	require.NoError(t, os.Chtimes(notAHome, old, old))

	report, err := Run(opts)
	require.NoError(t, err)
	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1)
	require.Contains(t, findings[0].Detail, stale)
	require.True(t, findings[0].Fixed, "fix outcome: %v", findings[0].FixErr)

	require.NoDirExists(t, stale, "stale home must be removed by --fix")
	require.DirExists(t, fresh, "recently-touched home must be spared")
	require.DirExists(t, notAHome, "a plain directory without AF markers must never be touched")
}

// TestInUseTempHomeSpared: a temp home referenced by a live process's
// AGENT_FACTORY_HOME is in use, no matter how old it is.
func TestInUseTempHomeSpared(t *testing.T) {
	testguard.IsolateTmux(t)

	// Spawn first so the home exists before the options' TempDir scan.
	tempRoot := t.TempDir()
	dir := filepath.Join(tempRoot, "tmp.in-use-home")
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0644))
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{filepath.Join(dir, "config.json"), filepath.Join(dir, "state.json"), dir} {
		require.NoError(t, os.Chtimes(p, old, old))
	}
	user := spawnWithEnv(t, "sh", nil, map[string]string{"AGENT_FACTORY_HOME": dir})

	opts := testOptions(t, true, user.PID)
	opts.TempDir = tempRoot
	report, err := Run(opts)
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "stale-temp-home"))
	require.DirExists(t, dir, "an in-use home must never be removed")
}

// TestForeignDaemonHandling: a daemon serving a deleted home is killable
// with --fix; one serving an existing (but different) home is report-only.
func TestForeignDaemonHandling(t *testing.T) {
	testguard.IsolateTmux(t)

	// argv0 "af" + a --daemon token makes the cmdline match the daemon
	// shape while really just being our own sleeping shell.
	deadHome := filepath.Join(t.TempDir(), "gone")
	broken := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": deadHome})

	liveHome := t.TempDir()
	intentional := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": liveHome})

	report, err := Run(testOptions(t, true, broken.PID, intentional.PID))
	require.NoError(t, err)
	findings := findByCheck(report, "foreign-daemon")
	require.Len(t, findings, 2)

	var fixed, reported int
	for _, f := range findings {
		if f.Fixed {
			fixed++
			require.Contains(t, f.Detail, deadHome)
		} else {
			reported++
			require.Empty(t, f.FixAction)
			require.Contains(t, f.Detail, liveHome)
		}
	}
	require.Equal(t, 1, fixed, "the daemon with a deleted home must be killed")
	require.Equal(t, 1, reported, "the daemon with an existing home must be left alone")
	require.Eventually(t, func() bool { return !alive(broken) }, 5*time.Second, 25*time.Millisecond)
	require.True(t, alive(intentional))
}

func TestForeignDaemonStatErrorIsNotReportedAsMissing(t *testing.T) {
	testguard.IsolateTmux(t)

	loopHome := filepath.Join(t.TempDir(), "loop-home")
	require.NoError(t, os.Symlink(loopHome, loopHome))
	uncertain := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": loopHome})

	report, err := Run(testOptions(t, true, uncertain.PID))
	require.NoError(t, err)
	findings := findByCheck(report, "foreign-daemon")
	require.Len(t, findings, 1)
	require.Contains(t, findings[0].Detail, loopHome)
	require.Contains(t, findings[0].Detail, "status cannot be verified")
	require.NotContains(t, findings[0].Detail, "no longer exists")
	require.Empty(t, findings[0].FixAction, "non-ENOENT stat errors must be report-only")
	require.False(t, findings[0].Fixed)
	require.True(t, alive(uncertain), "an uncertain foreign daemon must survive --fix")
}

// TestCleanRunHasNoFindings: an empty environment yields a clean bill of
// health and exit-0 semantics.
func TestCleanRunHasNoFindings(t *testing.T) {
	testguard.IsolateTmux(t)
	report, err := Run(testOptions(t, false))
	require.NoError(t, err)
	require.Empty(t, report.Findings)
	require.Zero(t, report.UnresolvedCount())

	var buf bytes.Buffer
	Render(&buf, report, false, false)
	require.Contains(t, buf.String(), "Summary:")
	require.Contains(t, buf.String(), "no issues require action")
}

// TestRenderShapes covers the three finding render states.
func TestRenderShapes(t *testing.T) {
	r := &Report{
		OK: []string{"daemon: not running (starts on demand)"},
		Findings: []Finding{
			{Check: "orphaned-process", Detail: "pid 1234 (yes)", FixAction: "kill pid 1234"},
			{Check: "leaked-tmux-session", Detail: "tmux session af_x has no backing record"},
			{Check: "stale-temp-home", Detail: "abandoned home /tmp/x", FixAction: "remove /tmp/x", Fixed: true},
		},
	}
	var buf bytes.Buffer
	Render(&buf, r, false, false)
	out := buf.String()
	require.Contains(t, out, "Agent Factory Doctor")
	require.Contains(t, out, "orphaned-processes")
	require.Contains(t, out, "1 safe to clean")
	require.Contains(t, out, "leaked-tmux-session")
	require.Contains(t, out, "FIXED stale-temp-home")
	require.Contains(t, out, "1 fixable with `af doctor --fix`")

	buf.Reset()
	Render(&buf, r, false, true)
	out = buf.String()
	require.Contains(t, out, "orphaned-process")
	require.Contains(t, out, "run `af doctor --fix` to kill pid 1234")
}

// TestTmuxServerDeadParsing pins the conservative TMUX-env heuristics.
func TestTmuxServerDeadParsing(t *testing.T) {
	testguard.RequireProcFS(t)
	self, err := proctree.Snapshot()
	require.NoError(t, err)
	ctx := &scanContext{snap: self}

	require.False(t, tmuxServerDead(ctx, "garbage"), "unparseable TMUX values are never accused")
	require.False(t, tmuxServerDead(ctx, "/tmp/sock,notanumber,0"))
	// A PID that certainly exists but is not tmux, with no socket on disk.
	require.True(t, tmuxServerDead(ctx, fmt.Sprintf("/nonexistent-sock-%d,%d,0", os.Getpid(), os.Getpid())))
	// A dead PID.
	c := exec.Command("true")
	require.NoError(t, c.Run())
	require.True(t, tmuxServerDead(ctx, fmt.Sprintf("/nonexistent-sock,%d,0", c.Process.Pid)))
}

// TestOrphanSignalIdentityGuard: even a fixable finding must refuse to fire
// when the pid has been recycled (the fix closure re-verifies identity).
func TestOrphanSignalIdentityGuard(t *testing.T) {
	testguard.RequireProcFS(t)
	testguard.IsolateTmux(t)
	home := t.TempDir()
	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		"AF_SESSION": "af_doctor-recycle",
		"AF_HOME":    home,
	})

	opts := testOptionsWithHome(t, home, false, orphan.PID)
	report, err := Run(opts)
	require.NoError(t, err)
	require.Len(t, findByCheck(report, "orphaned-process"), 1)

	// Kill the orphan out from under doctor, then apply the recorded fix:
	// the identity check must turn it into a no-op rather than a signal to
	// whoever owns the pid next.
	require.NoError(t, proctree.Signal(orphan, syscall.SIGKILL))
	require.Eventually(t, func() bool { return !alive(orphan) }, 5*time.Second, 10*time.Millisecond)

	f := findByCheck(report, "orphaned-process")[0]
	require.NotNil(t, f.fix)
	require.NoError(t, f.fix(), "a vanished process is a successfully-reaped process")
}

// writeHookScript writes an executable hook script under dir and returns its
// absolute path. body should be a complete shell script (with shebang).
func writeHookScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

// okContains reports whether any OK line contains sub.
func okContains(r *Report, sub string) bool {
	for _, line := range r.OK {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// withRemote returns opts wired to resolve the given hooks (repoRoot empty:
// the check falls back to a generic "[remote_hooks]" hint).
func withRemote(opts Options, hooks *config.RemoteHooks) Options {
	opts.remoteConfig = func() (*config.RemoteHooks, string, error) { return hooks, "", nil }
	return opts
}

// TestRemoteChecksSkippedWhenNoRemote: local-only users (no remote backend
// configured) get a single informational OK line and zero remote findings —
// running `af doctor` outside a remote setup adds no new noise.
func TestRemoteChecksSkippedWhenNoRemote(t *testing.T) {
	testguard.IsolateTmux(t)
	report, err := Run(testOptions(t, false)) // default resolver => nil hooks
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "remote-config"))
	require.Empty(t, findByCheck(report, "remote-hook-script"))
	require.Empty(t, findByCheck(report, "remote-connectivity"))
	require.True(t, okContains(report, "remote hooks: not configured"),
		"a clean n/a line must be shown for local-only users")
}

// TestRemoteConfigValidLaunchDelete: a config with executable launch_cmd +
// delete_cmd validates cleanly and emits the success OK line with no findings.
func TestRemoteConfigValidLaunchDelete(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	good := writeHookScript(t, dir, "hook.sh", "#!/bin/sh\necho '[]'\n")
	hooks := &config.RemoteHooks{LaunchCmd: good, DeleteCmd: good}

	report, err := Run(withRemote(testOptions(t, false), hooks))
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "remote-config"))
	require.Empty(t, findByCheck(report, "remote-hook-script"))
	require.True(t, okContains(report, "remote config: launch_cmd + delete_cmd configured"))
	require.Zero(t, report.UnresolvedCount())
}

// TestRemoteConfigMissingRequiredField: a remote config missing a required
// command (launch_cmd) is reported with the exact field name and where to fix
// it, mirroring the backend's own validation.
func TestRemoteConfigMissingRequiredField(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	good := writeHookScript(t, dir, "hook.sh", "#!/bin/sh\necho '[]'\n")
	hooks := &config.RemoteHooks{LaunchCmd: "", DeleteCmd: good}

	report, err := Run(withRemote(testOptions(t, false), hooks))
	require.NoError(t, err)
	checks := findCheckRows(report, "remote config")
	require.Len(t, checks, 1)
	require.Equal(t, StatusWarn, checks[0].Status)
	require.Contains(t, checks[0].Detail, "launch_cmd is required")
	require.Contains(t, checks[0].Detail, "remote_hooks")
	require.Zero(t, report.UnresolvedCount(), "remote warnings must not fail doctor")
}

// TestRemoteConfigMigrationKeyRejected: a config still carrying a removed
// pre-PR7 key (attach_cmd) fails the Validate guard with the provision-and-expose
// migration message, so `af doctor` names the exact stale key after an upgrade.
func TestRemoteConfigMigrationKeyRejected(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	good := writeHookScript(t, dir, "hook.sh", "#!/bin/sh\necho '[]'\n")
	hooks := &config.RemoteHooks{LaunchCmd: good, DeleteCmd: good, RemovedAttachCmd: "/bin/attach"}

	report, err := Run(withRemote(testOptions(t, false), hooks))
	require.NoError(t, err)
	checks := findCheckRows(report, "remote config")
	require.Len(t, checks, 1)
	require.Equal(t, StatusWarn, checks[0].Status)
	require.Contains(t, checks[0].Detail, "attach_cmd")
	require.Contains(t, checks[0].Detail, "was removed in the provision-and-expose migration")
	require.Zero(t, report.UnresolvedCount(), "remote warnings must not fail doctor")
}

// TestRemoteHookScriptNotExecutable: a hook path that exists but lacks the
// execute bit is flagged with the exact chmod fix.
func TestRemoteHookScriptNotExecutable(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	good := writeHookScript(t, dir, "hook.sh", "#!/bin/sh\necho '[]'\n")
	noexec := filepath.Join(dir, "launch.sh")
	require.NoError(t, os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644))
	hooks := &config.RemoteHooks{LaunchCmd: noexec, DeleteCmd: good}

	report, err := Run(withRemote(testOptions(t, false), hooks))
	require.NoError(t, err)
	checks := findCheckRows(report, "remote hook")
	require.Len(t, checks, 1)
	require.Equal(t, StatusWarn, checks[0].Status)
	require.Contains(t, checks[0].Detail, "not executable")
	require.Contains(t, checks[0].Detail, "chmod +x")
	require.Zero(t, report.UnresolvedCount(), "remote warnings must not fail doctor")
}

// TestRemoteHookScriptMissing: a hook path that does not exist is flagged.
func TestRemoteHookScriptMissing(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	good := writeHookScript(t, dir, "hook.sh", "#!/bin/sh\necho '[]'\n")
	missing := filepath.Join(dir, "does-not-exist.sh")
	hooks := &config.RemoteHooks{LaunchCmd: missing, DeleteCmd: good}

	report, err := Run(withRemote(testOptions(t, false), hooks))
	require.NoError(t, err)
	checks := findCheckRows(report, "remote hook")
	require.Len(t, checks, 1)
	require.Equal(t, StatusWarn, checks[0].Status)
	require.Contains(t, checks[0].Detail, "does not exist")
	require.Contains(t, checks[0].Detail, "launch_cmd")
	require.Zero(t, report.UnresolvedCount(), "remote warnings must not fail doctor")
}

func TestRemoteCoderWhoamiWarnsWithoutFailingDoctor(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	binDir := t.TempDir()
	writeExecutable(t, binDir, "coder", "#!/bin/sh\nif [ \"$1\" = \"whoami\" ]; then echo 'not logged in' >&2; exit 1; fi\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hook := writeHookScript(t, dir, "coder-hook.sh", "#!/bin/sh\necho '[]'\n")
	hooks := &config.RemoteHooks{LaunchCmd: hook, DeleteCmd: hook}

	report, err := Run(withRemote(testOptions(t, false), hooks))
	require.NoError(t, err)
	checks := findCheckRows(report, "coder")
	require.Len(t, checks, 1)
	require.Equal(t, StatusWarn, checks[0].Status)
	require.Contains(t, checks[0].Detail, "not logged in")
	require.Equal(t, "run `coder login`", checks[0].Remediation)
	require.Zero(t, report.UnresolvedCount(), "coder auth warnings must not fail doctor")
}
