package integration_test

import (
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

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestDockerBackendAccountWorkspaceBindNotReowned is the end-to-end proof for
// procedure #14 of the test plan: a repo-controlled `docker.run_args` mount at
// `/workspace` MUST NOT be re-owned by `prepareAccountUser`'s root chown on a
// rootful docker engine. Before the fix the account path ran
// `chown <owner> /workspace` unconditionally, which re-owned the operator's
// host directory through the bind (weakening, e.g., a root-owned directory to
// the af account uid). After the fix the mount-table guard detects `/workspace`'s
// enclosing mount is not the container root and skips the chown with a warning.
//
// It stands up the real round-trip docker image, a credential account the
// operator "owns", a repo configured to mount a host directory at `/workspace`
// via `docker.run_args`, and provisions the session through the ordinary
// NewInstance path. It then asserts the host directory's owner is UNCHANGED
// after provisioning. Before the fix this would have flipped to the account
// owner on a rootful engine; after the fix the mount-table guard skips the
// chown, so the host directory stays exactly as it was.
//
// A fake "claude" agent binary is bind-mounted into the container so the
// in-container agent-server can complete its startup; the test does not drive
// the agent, it only needs the container to be far enough into provisioning
// that the chown has either run (the bug) or been skipped (the fix).
//
// Runs only on a host with a real rootful docker daemon; `requireDocker` and
// the daemon's own rootless check skip cleanly otherwise. SKIPS under
// `testing.Short()`, mirroring the other real-docker E2E tests. The fixture's
// ownership change runs through a root container on the same daemon, so the
// test process itself does NOT need host root — a non-root CI runner against
// a rootful daemon exercises the guard for real.
func TestDockerBackendAccountWorkspaceBindNotReowned(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive real-backend E2E/integration; skipped under -short — see #2052")
	}
	requireDocker(t)
	requireTool(t, "git")
	baseImage := requireRoundTripBaseImage(t)

	// Rootful engines are where the chown-through-bind re-own hazard
	// manifests. On a rootless engine the chown executes as uid 0 in the
	// userns and maps to the operator's own uid, so the host-file re-own is
	// a no-op and the test cannot distinguish "fixed" from "no-op". Query
	// the daemon's own rootless flag (`docker info` reports `name=rootless`
	// in SecurityOptions — the same signal the capacity probe's
	// parseDockerEngineInfo uses) rather than the CLI client's EUID: a
	// non-root client talking to a rootful system daemon must NOT be
	// skipped (the daemon has exactly the chown behavior this test
	// exercises), and a root client talking to a rootless daemon must NOT
	// run (the chown is a no-op there).
	if daemonRootless(t) {
		t.Skip("this proof requires a rootful docker engine (daemon as root); the daemon reports name=rootless in SecurityOptions")
	}
	// No host-root check here: the account-dir fixture chown below runs
	// through a root container on the same daemon, so the test only needs
	// what it skipped on above — a reachable rootful engine — plus the
	// bind-source visibility check after the fixture runs.

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// The pane shim resolves an account NAME against this machine's AF home;
	// main.go installs this hook for the real binary. Install it here so an
	// account-scoped launch reaches the provision step instead of refusing
	// outright at lookup.
	previousLookup := sessionenv.AccountLookup
	sessionenv.AccountLookup = func(agent, name string) (sessionenv.Account, error) {
		return agentaccount.Selected(home, agent, name)
	}
	t.Cleanup(func() { sessionenv.AccountLookup = previousLookup })

	// The operator's registered account. We chown the account directory to
	// a NON-ROOT uid (nobody:65534) after registration. That makes the
	// /af-account stat probe in prepareAccountUser report 65534:65534
	// inside the container, so the chown of /workspace would re-own the
	// host's root:root directory to nobody:nogroup through the bind on
	// master — the bug. On the fix the chown is skipped, so the host
	// directory's owner is unchanged.
	//
	// This mirrors the bug report's exploitable scenario: chown writes the
	// account owner (= probed stat) through the bind onto a host directory
	// that did not previously have that owner.
	accountDir, err := agentaccount.Register(home, "claude", "work")
	if err != nil {
		t.Fatalf("register account: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(accountDir, ".config"), 0o700); err != nil {
		t.Fatalf("seed account config dir: %v", err)
	}
	writeFile(t, filepath.Join(accountDir, ".config", "settings.json"), "LEGIT\n", 0o600)
	// Make the account directory owned by a non-root uid so the in-container
	// stat probe reports that uid and the chown of /workspace would re-own
	// the operator's root:root host directory to it (the bug).
	//
	// The test process does not have the privilege this chown needs (CI runs
	// as a non-root user whose daemon is rootful), so it runs through a root
	// container on the same rootful engine: container-root re-owns the
	// bind-mounted host directory exactly as a host-root chown would.
	chownCtx, chownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	out, err := exec.CommandContext(chownCtx, "docker", "run", "--rm",
		"-v", accountDir+":/af-account-fixture",
		baseImage, "chown", "-R", "65534:65534", "/af-account-fixture").CombinedOutput()
	chownCancel()
	if err != nil {
		t.Fatalf("fixture chown of the account dir through a root container failed: %v\n%s", err, out)
	}
	// A bind source resolves on the DAEMON host: when the engine is not local
	// the chown landed somewhere this test cannot see, and the owner check
	// below would measure nothing. Verify the fixture actually took before
	// relying on it — that is exactly what this test needs.
	if got := fileOwner(t, accountDir); got != "65534:65534" {
		t.Skipf("the root-container fixture chown did not take on the host account dir (owner is %s): the daemon resolves bind sources on a different host", got)
	}

	// A static `af` binary the runtime copies into the container.
	defer session.SetDockerSelfBinaryForTest(buildStaticBinary(t))()

	// A host directory the operator owns (root:root) which the repo will
	// bind at /workspace. We assert its owner:group is UNCHANGED by the
	// provision attempt. tmpdir to keep the test self-contained.
	hostWorkspace := filepath.Join(t.TempDir(), "host-workspace")
	if err := os.MkdirAll(hostWorkspace, 0o755); err != nil {
		t.Fatalf("mkdir host workspace: %v", err)
	}
	wantOwner := fileOwner(t, hostWorkspace)

	// A fake "claude" agent the in-container agent-server will exec. The
	// agent-server process itself is the copied `af` binary running
	// `af agent-server --program claude`, which then exec's the bare
	// `claude` name from PATH. A no-op shell script satisfies PATH lookup
	// without needing a real claude install; the agent-server will still
	// print its startup banner once its listener binds, which is what
	// readBanner polls for.
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	writeFile(t, filepath.Join(fakeBin, "claude"),
		"#!/bin/sh\nsleep 3600\n", 0o755)

	image := buildDockerRoundTripImage(t)

	// The source repo + a bare clone bind-mounted at /repo.git for the
	// in-container clone (mirrors the round-trip test pattern).
	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "workspace-bind\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")

	// Repo config: backend=docker + the bind-mount at /workspace AND the
	// bare clone source /repo.git (read-only) AND the fake claude bound at a
	// fixed PATH location. The /workspace bind is the repo-controlled
	// run_args case the account run_args screen accepts and the chown guard
	// must skip. A repo `-e PATH=` to reach the fake agent is not an option:
	// the account run_args screen refuses it, so the fake binary is bound
	// where the image's own PATH already looks.
	//
	// With the fix the /workspace chown is skipped, so when the clone runs
	// later as the account user (uid 65534) against a root:root /workspace,
	// it FAILS with "Permission denied" — exactly the WARNED-about outcome
	// the chown-skip's warning message names ("make sure it is writable by
	// that user"). The test asserts that the host directory's OWNER is
	// UNCHANGED and that the failure (if any) is the clone's permission
	// denial, NOT a chown-induced re-own.
	writeDockerRepoConfig(t, repo, image, []string{
		"-v", bare + ":/repo.git:ro",
		"-v", hostWorkspace + ":/workspace",
		"-v", filepath.Join(fakeBin, "claude") + ":/usr/local/bin/claude:ro",
	})

	title := "docker-account-workspace-bind"
	slug := session.Slugify(title)
	t.Cleanup(func() { reapByLabel(slug) })

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    repo,
		Program: "claude",
		Account: "work",
		Backend: session.BackendDocker,
	})

	// The behavioral proof: the host directory's owner:group is UNCHANGED
	// regardless of whether provisioning succeeded. Before the fix,
	// `prepareAccountUser` would have run `chown 65534:65534 /workspace`
	// through the bind before the clone ran, re-owning the operator's
	// root:root host directory to nobody:nogroup; then the clone would
	// have succeeded and written attacker-controlled content into it.
	// After the fix, the chown is skipped and the clone runs as the
	// account user against a root:root /workspace it cannot write to, so
	// it fails with "Permission denied" (the warned-about outcome), and
	// the host directory's owner is preserved.
	gotOwner := fileOwner(t, hostWorkspace)
	if gotOwner != wantOwner {
		t.Fatalf("prepareAccountUser re-owned the operator's host directory through the /workspace bind: want %s, got %s — the mount-table guard should have skipped the chown", wantOwner, gotOwner)
	}
	t.Logf("host.workspace %s owner unchanged after provisioning (was=%s is=%s): the mount-table guard skipped the chown of the bind-mounted /workspace", hostWorkspace, wantOwner, gotOwner)

	if err != nil {
		// Under the R6 contract, the provision refuses at the chown-guard
		// step: /workspace is inside a host bind the daemon's inspect record
		// confirms, and the account user (uid 65534) cannot write a
		// root:root directory that was NOT re-owned. The session's ERROR
		// must name the mount, its host source, and the remedy — the
		// operator should never be handed a bare clone "Permission denied"
		// to debug.
		for _, want := range []string{hostWorkspace, "/workspace", "bind", "chown"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("an unwritable host bind must refuse with %q named in the error; got: %v", want, err)
			}
		}
		t.Logf("provision refused at the chown guard, naming the mount and the remedy: %v", err)
		// Procedure #15: the prepareAccountUser skip warning lands in the
		// host's agent-factory.log under AGENT_FACTORY_HOME when
		// log.Initialize has run. This in-process test path does not call
		// log.Initialize (a daemon would), so the log is not at the home
		// path here — the dedicated unit test
		// TestPrepareAccountUser_SkippedChownLogsWarning in ./session/
		// asserts the warning text via log.CaptureOutput instead, which
		// is the supported way to observe log output from a test. The
		// host-file landing of the warning is exercised by the daemon-led
		// suite (e.g. sandbox_disruption_e2e_test.go reads
		// $AGENT_FACTORY_HOME/agent-factory.log); that is a different
		// test path and not a property of the chown guard itself.
		return
	}
	defer func() {
		if inst != nil {
			_ = inst.Kill()
		}
	}()

	// If the clone succeeded (e.g. because /workspace was world-writable
	// from a different setup), the container must be up — and the host
	// directory's owner must STILL be the original owner.
	if got := containersForLabel(t, slug); len(got) == 0 {
		t.Fatal("expected a running container for the docker session after provisioning")
	}
	t.Logf("session provisioned; the host directory's owner is preserved as %s", gotOwner)
}

// fileOwner reports the "uid:gid" of path via stat(1). The test runs on the
// host where the docker daemon also runs (rootful), so this is the same
// filesystem inode the in-container chown would have targeted through the
// bind. GNU stat spells the format flag -c; BSD stat (a macOS host driving
// its VM daemon) spells it -f — both render uid:gid the same way.
func fileOwner(t *testing.T, path string) string {
	t.Helper()
	formatFlag := "-c"
	if runtime.GOOS == "darwin" {
		formatFlag = "-f"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "stat", formatFlag, "%u:%g", path).CombinedOutput()
	if err != nil {
		t.Fatalf("stat %s: %v\n%s", path, err, out)
	}
	return strings.TrimSpace(string(out))
}

// daemonRootless reports whether the reachable docker daemon is rootless by
// querying `docker info`'s SecurityOptions for `name=rootless` — the same
// signal the capacity probe (session.parseDockerEngineInfo) uses. Detecting
// rootlessness via the daemon (not the CLI client's EUID) keeps the test from
// skipping on a non-root client talking to a rootful system daemon, and from
// running on a root client talking to a rootless daemon.
func daemonRootless(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .}}").CombinedOutput()
	if err != nil {
		t.Skipf("docker info not reachable; skipping the rootful-engine proof: %s", strings.TrimSpace(string(out)))
	}
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		return false
	}
	var raw struct {
		SecurityOptions []string `json:"SecurityOptions"`
	}
	if err := json.NewDecoder(strings.NewReader(string(out)[start:])).Decode(&raw); err != nil {
		return false
	}
	for _, opt := range raw.SecurityOptions {
		if opt == "name=rootless" {
			return true
		}
	}
	return false
}

// hold these references so `go vet` does not flag them as unused if the test
// skips at build time on a non-linux/non-darwin box (it would not, but keep the
// intent explicit).
var (
	_ = fmt.Sprintf
)
