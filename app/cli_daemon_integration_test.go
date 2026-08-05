package app

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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestTUIRefreshSeesCLIChangesThroughDaemon(t *testing.T) {
	skipIfRealBackendDepsMissing(t)

	// #1056: private tmux server for the exec'd af CLI, its daemon, and the
	// in-process TmuxAlive probes (all inherit the environment), so no af_
	// session can leak onto the developer's server.
	testguard.IsolateTmux(t)

	bin := buildIntegrationBinary(t)
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID
	h.repoRoot = repo.Root
	writeIntegrationConfig(t, os.Getenv("AGENT_FACTORY_HOME"))

	t.Cleanup(func() {
		_, _ = runIntegrationAF(t, bin, repoDir, "sessions", "kill", "cli-made")
		killIntegrationDaemon(os.Getenv("AGENT_FACTORY_HOME"))
	})

	runIntegrationAFOK(t, bin, repoDir, "sessions", "--repo", repoDir, "create", "--name", "cli-made", "--program", tmux.ProgramClaude)
	require.True(t, reconcileFromDaemon(t, h), "TUI snapshot should import CLI-created session")
	require.NotNil(t, findSidebarInstance(h, "cli-made"))

	runIntegrationAFOK(t, bin, repoDir, "sessions", "kill", "cli-made")
	require.True(t, reconcileFromDaemon(t, h), "TUI snapshot should remove CLI-killed session")
	require.Nil(t, findSidebarInstance(h, "cli-made"))
}

// TestTUIRefreshSwapsKillRecreatedSameTitle is the regression test for #765.
//
// When a session is killed and recreated under the SAME title via the CLI
// WITHOUT an intervening refresh, the sidebar holds a dead in-memory instance
// while a fresh, live instance exists on disk. The old title-only
// reconciliation skipped both the add (title already present) and the remove
// (title still on disk), so the corpse permanently shadowed the new session:
// the user could neither attach nor preview it. refreshExternalInstances must
// detect the stale instance (its tmux session is gone) and swap it for the
// recreated on-disk one.
func TestTUIRefreshSwapsKillRecreatedSameTitle(t *testing.T) {
	skipIfRealBackendDepsMissing(t)

	// #1056: private tmux server for the exec'd af CLI, its daemon, and the
	// in-process TmuxAlive probes (all inherit the environment), so no af_
	// session can leak onto the developer's server.
	testguard.IsolateTmux(t)

	bin := buildIntegrationBinary(t)
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID
	h.repoRoot = repo.Root
	writeIntegrationConfig(t, os.Getenv("AGENT_FACTORY_HOME"))

	t.Cleanup(func() {
		_, _ = runIntegrationAF(t, bin, repoDir, "sessions", "kill", "recreated")
		killIntegrationDaemon(os.Getenv("AGENT_FACTORY_HOME"))
	})

	// Create the session and import it into the sidebar.
	runIntegrationAFOK(t, bin, repoDir, "sessions", "--repo", repoDir, "create", "--name", "recreated", "--program", tmux.ProgramClaude)
	require.True(t, reconcileFromDaemon(t, h), "TUI snapshot should import CLI-created session")
	original := findSidebarInstance(h, "recreated")
	require.NotNil(t, original)
	require.True(t, original.TmuxAlive(), "imported instance should be alive")

	// Kill then recreate the SAME title via the CLI, with NO refresh in
	// between. The in-memory instance now points at a dead tmux session while
	// a brand-new live instance sits on disk under the same title.
	runIntegrationAFOK(t, bin, repoDir, "sessions", "kill", "recreated")
	require.False(t, original.TmuxAlive(), "killed instance's tmux session must be gone")
	runIntegrationAFOK(t, bin, repoDir, "sessions", "--repo", repoDir, "create", "--name", "recreated", "--program", tmux.ProgramClaude)

	require.True(t, reconcileFromDaemon(t, h), "TUI snapshot should swap the stale instance")

	// Exactly one "recreated" instance, and it must be the new live one — not
	// the dead corpse we started with.
	var matches []*session.Instance
	for _, inst := range h.store.GetInstances() {
		if inst.Title == "recreated" {
			matches = append(matches, inst)
		}
	}
	require.Len(t, matches, 1, "sidebar must hold exactly one instance for the reused title")
	swapped := matches[0]
	require.NotSame(t, original, swapped, "sidebar must hold the recreated instance, not the dead one")
	require.True(t, swapped.TmuxAlive(), "swapped-in instance must be attachable (live tmux session)")
}

func findSidebarInstance(h *home, title string) *session.Instance {
	for _, inst := range h.store.GetInstances() {
		if inst.Title == title {
			return inst
		}
	}
	return nil
}

// reconcileFromDaemon fetches the daemon's authoritative snapshot for the home's
// repo and reconciles the sidebar to it — the TUI's single sync path (#960 PR 4).
// It replaces the deleted disk-based refreshExternalInstances in these
// CLI→daemon→TUI integration checks.
func reconcileFromDaemon(t *testing.T, h *home) bool {
	t.Helper()
	resp, err := snapshotThroughDaemon(h.repoID)
	require.NoError(t, err)
	changed := h.reconcileSnapshot(resp.Instances)
	if h.applyDeliveryAlarms(resp.DeliveryAlarms) {
		changed = true
	}
	return changed
}

func buildIntegrationBinary(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	repoRoot := filepath.Dir(filepath.Dir(file))
	bin := filepath.Join(t.TempDir(), "af")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, repoRoot)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go build failed:\n%s", out)
	return bin
}

// runIntegrationAFOK runs af and fails the test if it errors — but RETRIES for as
// long as the daemon answers that it is still restoring sessions.
//
// That answer is not a failure, it is an instruction: the documented, retryable
// warm-up state, which the daemon publishes precisely so a client can come back.
// The FIRST call in one of these tests is what auto-starts the daemon, so it
// races the daemon's own restore, and turning that race into require.NoError
// reddened a PR whose diff was TypeScript (#2863).
//
// The retry is keyed on the product's own classifier rather than a string this
// test keeps in step by hand, and on the CONDITION rather than a nap chosen to
// out-wait a restore. A refused call did nothing, so re-running it is safe.
func runIntegrationAFOK(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	var (
		out string
		err error
	)
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err = runIntegrationAF(t, bin, dir, args...)
		if !daemon.IsDaemonStartingErr(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("af %s: the daemon never finished restoring: %v", strings.Join(args, " "), err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, err)
	return out
}

// integrationWarmupWait bounds how long an integration CLI call waits out a
// daemon that is up but still restoring its instances.
//
// It is a readiness wait, not a widened command timeout: the 30s per-invocation
// bound below is unchanged, and only the one refusal that means "ask me again"
// is waited on. A few seconds is plenty — the daemon restores quickly, and the
// failure this exists for was on the very first call of a test.
// A var, not a const, only so the timeout-path test can shorten it; production
// test runs never reassign it.
var integrationWarmupWait = 30 * time.Second

// runIntegrationAF runs the built af binary, waiting out the daemon's warm-up
// window rather than racing it (#2863).
//
// The first CLI call of a test is what auto-starts the daemon, so it races that
// daemon's own session restore. In that window every state-dependent RPC is
// refused with daemonStartingErrText and the CLI exits 1 — and the helper went
// straight to require.NoError, so whether the test passed depended on how loaded
// the runner was. The failure then read as "the TUI cannot see CLI changes
// through the daemon" when what actually happened is that the CLI was told to
// retry and the test refused to.
//
// Retrying here is not a policy this test invents: IsDaemonStartingErr's own doc
// says callers should treat it as retryable, the refusal is an admission check
// that happens BEFORE any state-dependent work (see errDaemonStarting), so
// nothing was attempted and re-running is safe, and the TUI already does exactly
// this — TestColdStartFromSnapshot_WaitsOutWarmingDaemon pins that behavior. The
// helper was the one participant that did not.
//
// Deliberately narrow: it matches ONLY the warm-up refusal, via the product's own
// classifier, so a genuine CLI failure still fails immediately and loudly rather
// than being retried into a timeout. Nothing about what the callers assert
// changes.
func runIntegrationAF(t *testing.T, bin, dir string, args ...string) (string, error) {
	t.Helper()
	deadline := time.Now().Add(integrationWarmupWait)
	for attempt := 1; ; attempt++ {
		out, err := runIntegrationAFOnce(t, bin, dir, args...)
		if !daemon.IsDaemonStartingErr(err) {
			return out, err
		}
		if !time.Now().Before(deadline) {
			// Keep the underlying refusal in the chain: a timeout here must still
			// say WHY it gave up, not just that it did.
			return out, fmt.Errorf("af %s: daemon was still restoring after %s (%d attempts): %w",
				strings.Join(args, " "), integrationWarmupWait, attempt, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func runIntegrationAFOnce(t *testing.T, bin, dir string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), fmt.Errorf("timed out running af %s; stderr=%s", strings.Join(args, " "), stderr.String())
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("%w; stderr=%s stdout=%s", err, stderr.String(), stdout.String())
	}
	return stdout.String(), nil
}

func writeIntegrationConfig(t *testing.T, home string) {
	t.Helper()
	// Write a wrapper script that ignores any --plugin-dir / --dangerously-skip
	// flags injectSystemPrompt would append; route the claude enum to it via
	// ProgramOverrides so the CLI accepts --program claude (#658).
	//
	// It prints a "❯" ready prompt before reading stdin so the daemon's
	// waitForReady loop recognises the pane as ready. The create path now
	// always waits for readiness — even for empty-prompt sessions (#698) — so
	// a real agent's startup prompt must be emulated here.
	wrapper := filepath.Join(home, "fake-agent.sh")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf '❯ '\nexec cat\n"), 0755))
	cfg := &config.Config{
		DefaultProgram:     tmux.ProgramClaude,
		ProgramOverrides:   map[string]string{tmux.ProgramClaude: wrapper},
		DaemonPollInterval: 100,
		BranchPrefix:       "test/",
		DetachKeys:         "ctrl-w",
	}
	require.NoError(t, config.SaveConfig(cfg))
}

func killIntegrationDaemon(home string) {
	raw, err := os.ReadFile(filepath.Join(home, "daemon.pid"))
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(string(raw), "%d", &pid); err != nil || pid <= 1 {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
}

// TestTUIRefreshDoesNotSwapLoadingPlaceholder is the regression test for #808.
//
// The daemon persists a new session (and lists it in its snapshot) BEFORE the
// create RPC returns, so while the TUI's Loading placeholder is still in the
// sidebar, the snapshot already carries the same title with a newer CreatedAt.
// A naive #765 swap would treat that as a CLI kill+recreate and swap the
// placeholder out; the instanceStartedMsg handler would then miss it
// (pointer-based ReplaceInstance/ContainsInstance) and re-add the started
// instance, leaving two same-title sidebar rows. reconcileSnapshot prevents this
// by skipping transient (Loading/Deleting) rows entirely.
func TestTUIRefreshDoesNotSwapLoadingPlaceholder(t *testing.T) {
	skipIfRealBackendDepsMissing(t)
	// #1122: this test runs a real `af sessions create` (real daemon, real
	// tmux). Without a private server the "scripts" session lives on the
	// ambient server for the test's lifetime — the #1121 CI run's daemon
	// package tripwired on exactly that transient session.
	testguard.IsolateTmux(t)

	bin := buildIntegrationBinary(t)
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID
	h.repoRoot = repo.Root
	writeIntegrationConfig(t, os.Getenv("AGENT_FACTORY_HOME"))

	t.Cleanup(func() {
		_, _ = runIntegrationAF(t, bin, repoDir, "sessions", "kill", "scripts")
		killIntegrationDaemon(os.Getenv("AGENT_FACTORY_HOME"))
	})

	// The TUI's placeholder for an in-flight create of "scripts".
	placeholder, err := session.NewInstance(session.InstanceOptions{
		Title:   "scripts",
		Path:    repoDir,
		Program: tmux.ProgramClaude,
	})
	require.NoError(t, err)
	placeholder.SetStatusForTest(session.Loading)
	h.store.AddInstance(placeholder)

	// The daemon persists the session record mid-create — emulated by a CLI
	// create, which goes through the same daemon CreateSession path the TUI
	// start RPC uses.
	runIntegrationAFOK(t, bin, repoDir, "sessions", "--repo", repoDir, "create", "--name", "scripts", "--program", tmux.ProgramClaude)

	// A snapshot reconcile fires while the placeholder is still Loading. It must
	// not swap the placeholder out from under the in-flight create: reconcileSnapshot
	// skips transient (Loading/Deleting) rows entirely (#808).
	require.False(t, reconcileFromDaemon(t, h), "snapshot reconcile must leave the Loading placeholder alone")
	require.Same(t, placeholder, findSidebarInstance(h, "scripts"),
		"the Loading placeholder must stay in the sidebar until its start completes (#808)")

	// The start RPC completes, returning a freshly-built instance — exactly
	// what startSessionThroughDaemon produces via FromInstanceData.
	diskStore, err := session.NewStorage(config.DefaultState(), repo.ID)
	require.NoError(t, err)
	diskData, err := diskStore.LoadInstanceData()
	require.NoError(t, err)
	var rec *session.InstanceData
	for i := range diskData {
		if diskData[i].Title == "scripts" {
			rec = &diskData[i]
		}
	}
	require.NotNil(t, rec, "daemon must have persisted the session before the RPC returned")
	started, err := session.FromInstanceData(*rec)
	require.NoError(t, err)

	_, _ = h.Update(instanceStartedMsg{instance: placeholder, started: started})

	var matches []*session.Instance
	for _, inst := range h.store.GetInstances() {
		if inst.Title == "scripts" {
			matches = append(matches, inst)
		}
	}
	require.Len(t, matches, 1, "one logical session must occupy exactly one sidebar row (#808)")
	require.Same(t, started, matches[0])

	// The daemon — the sole writer (#960 PR 4) — already persisted exactly one
	// record on create; the TUI writes nothing. Assert the on-disk state directly.
	raw, err := config.DefaultState().GetInstances(repo.ID)
	require.NoError(t, err)
	var onDisk []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &onDisk))
	count := 0
	for _, d := range onDisk {
		if d.Title == "scripts" {
			count++
		}
	}
	require.Equal(t, 1, count, "instances.json must hold exactly one record for the title (#808)")
}

// fakeAFScript writes a stand-in for the af binary that runIntegrationAF can
// exec. It counts its invocations in a file so a test can prove how many times
// the helper actually ran it, then behaves as `body` dictates.
func fakeAFScript(t *testing.T, dir, body string) (script, counter string) {
	t.Helper()
	counter = filepath.Join(dir, "attempts")
	script = filepath.Join(dir, "fake-af")
	// The counter path is quoted: t.TempDir() embeds the test name, and an
	// unquoted path is one rename away from splitting on a space.
	src := "#!/bin/sh\n" +
		"n=$(cat '" + counter + "' 2>/dev/null || echo 0)\n" +
		"n=$((n+1)); echo $n > '" + counter + "'\n" +
		body
	require.NoError(t, os.WriteFile(script, []byte(src), 0o755))
	return script, counter
}

func fakeAFAttempts(t *testing.T, counter string) int {
	t.Helper()
	raw, err := os.ReadFile(counter)
	require.NoError(t, err)
	var n int
	_, err = fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &n)
	require.NoError(t, err)
	return n
}

// TestRunIntegrationAF_WaitsOutTheDaemonWarmUp is the #2863 regression guard.
//
// The first CLI call of an integration test auto-starts the daemon and therefore
// races its session restore; in that window the daemon refuses state-dependent
// RPCs with the warm-up error and the CLI exits 1. The helper used to hand that
// straight to require.NoError, so the test passed or failed on how loaded the
// runner was.
//
// errDaemonStarting() is the same literal cold_start_test.go uses, and its init()
// already asserts daemon.IsDaemonStartingErr recognizes it — so this cannot drift
// into testing a string the product no longer emits.
func TestRunIntegrationAF_WaitsOutTheDaemonWarmUp(t *testing.T) {
	dir := t.TempDir()
	script, counter := fakeAFScript(t, dir,
		"if [ $n -lt 3 ]; then\n"+
			"  echo '{\"error\":\""+errDaemonStarting().Error()+"\"}' >&2\n"+
			"  exit 1\n"+
			"fi\n"+
			"echo ready\n")

	out, err := runIntegrationAF(t, script, dir, "sessions", "list")
	require.NoError(t, err, "a daemon that is merely still restoring must be waited out, not failed on")
	assert.Equal(t, "ready\n", out)
	assert.Equal(t, 3, fakeAFAttempts(t, counter),
		"the helper must actually have retried; passing on the first attempt would prove nothing")
}

// TestRunIntegrationAF_DoesNotRetryARealFailure is the other half, and the one
// that keeps this from weakening every test using the helper: only the warm-up
// refusal is waited on. A genuine CLI failure must still fail immediately and
// loudly rather than being retried into a timeout.
func TestRunIntegrationAF_DoesNotRetryARealFailure(t *testing.T) {
	dir := t.TempDir()
	script, counter := fakeAFScript(t, dir,
		"echo '{\"error\":\"no such session\"}' >&2\n"+
			"exit 1\n")

	started := time.Now()
	_, err := runIntegrationAF(t, script, dir, "sessions", "kill", "nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such session", "the real error must survive to the caller")
	assert.Equal(t, 1, fakeAFAttempts(t, counter), "a real failure must be reported on the first attempt")
	assert.Less(t, time.Since(started), integrationWarmupWait,
		"a real failure must not be retried into the warm-up window")
}

// TestRunIntegrationAF_WarmUpTimeoutSaysWhy: if the daemon never finishes
// restoring, the helper must still report the refusal it gave up on rather than a
// bare timeout — otherwise the flake this fixes would come back as a mystery.
func TestRunIntegrationAF_WarmUpTimeoutSaysWhy(t *testing.T) {
	dir := t.TempDir()
	script, _ := fakeAFScript(t, dir,
		"echo '{\"error\":\""+errDaemonStarting().Error()+"\"}' >&2\n"+
			"exit 1\n")

	prev := integrationWarmupWait
	integrationWarmupWait = 150 * time.Millisecond
	t.Cleanup(func() { integrationWarmupWait = prev })

	_, err := runIntegrationAF(t, script, dir, "sessions", "list")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still restoring", "the timeout must name the condition it waited on")
	assert.Contains(t, err.Error(), errDaemonStarting().Error(), "and keep the underlying refusal")
}
