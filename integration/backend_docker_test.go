package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// TestDockerBackendRoundTrip is the #1592 Phase 4 PR4 flagship proof: a session
// runs in a REAL docker container, to parity with local.
//
// It builds a slim git+tmux+bash image, sets a repo's config to backend=docker,
// and creates a session through the ordinary NewInstance path — so the docker
// runtime runs a container, clones the workspace into it, docker cp's a static
// `af` binary in, starts an `af agent-server` on a published loopback port behind
// a bearer token, and exposes its http:// URL. The daemon-side Instance then drives that
// in-container agent-server over the wire through the FULL surface:
//
//	Start (Provision+Launch the in-container workspace) → Subscribe to its PTY
//	stream → Input (typed bytes reach the in-container agent) → observe the echo →
//	Preview / Snapshot / Alive reflect the pane → Kill → the container is reaped
//	(no leak).
//
// This is the whole epic's payoff: a session, end to end, inside a container,
// behaviorally indistinguishable from a local one.
//
// Run it on a host with docker: make backend-docker-roundtrip. It SKIPS cleanly
// where docker is unavailable (e.g. inside the test-container fence), so the full
// suite stays green there.
func TestDockerBackendRoundTrip(t *testing.T) {
	requireDocker(t)
	requireTool(t, "git")

	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	// A static `af` binary the runtime copies into the container (musl-compatible;
	// the daemon's own binary is what production copies, but the test binary is not
	// `af`, so build a real one and point the runtime at it).
	afBin := buildStaticBinary(t)
	restore := session.SetDockerSelfBinaryForTest(afBin)
	defer restore()

	image := buildDockerRoundTripImage(t)

	// The source repo + a bare clone of it, bind-mounted into the container as the
	// clone source (file:///repo.git). This makes the in-container clone a REAL git
	// clone with no GitHub dependency — the durable-store model, self-contained.
	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "docker round-trip\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")

	// Repo config: backend=docker, the test image, and the bind-mount that serves
	// the clone source at /repo.git inside the container.
	writeDockerRepoConfig(t, repo, image, []string{"-v", bare + ":/repo.git:ro"})

	title := "docker-rt"
	slug := session.Slugify(title)
	// Backstop cleanup: reap any container this test's label leaves behind even if
	// it fails before Kill, so a container never leaks out of the test.
	t.Cleanup(func() { reapByLabel(slug) })

	// --- create the session on the docker backend (the full NewInstance path) ---
	// program `cat` echoes the PTY, so typed input observably comes back over the
	// stream — the input-reaches-the-in-container-agent proof.
	t.Logf("provisioning docker session %q (image %s)...", title, image)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    repo,
		Program: "cat",
		Backend: session.BackendDocker,
	})
	if err != nil {
		t.Fatalf("NewInstance(backend=docker): %v", err)
	}
	// The container is up the instant NewInstance returns (the runtime provisioned
	// it). Assert it, and route every later failure through Kill so it is reaped.
	if got := containersForLabel(t, slug); len(got) == 0 {
		t.Fatal("expected a running container for the docker session after provisioning")
	}
	t.Logf("container is up; agent-server exposed over http:// with a bearer token")
	killed := false
	defer func() {
		if !killed {
			_ = inst.Kill()
		}
	}()

	// --- Start: Provision + Launch the in-container workspace over the wire ------
	if err := inst.Start(true); err != nil {
		t.Fatalf("Start (drive in-container agent-server Provision+Launch): %v", err)
	}

	as := inst.AgentServer()

	// --- Subscribe to the in-container PTY stream across the container boundary --
	sub, err := as.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe to the in-container PTY stream: %v", err)
	}
	defer func() { _ = sub.Close() }()

	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	defer pumpCancel()
	var (
		pumpMu sync.Mutex
		pump   strings.Builder
	)
	go func() {
		for {
			ev, rerr := sub.NextEvent(pumpCtx)
			if rerr != nil {
				return
			}
			if ev.Kind == session.PTYData || ev.Kind == session.PTYRepaint {
				pumpMu.Lock()
				pump.Write(ev.Data)
				pumpMu.Unlock()
			}
		}
	}()
	streamOutput := func() string {
		pumpMu.Lock()
		defer pumpMu.Unlock()
		return pump.String()
	}

	// --- Input: typed bytes reach the in-container agent; the `cat` pane echoes --
	marker := "docker-roundtrip-ping"
	if err := as.Input(0, []byte(marker+"\n")); err != nil {
		t.Fatalf("Input over the wire: %v", err)
	}
	waitUntil(t, 20*time.Second, "in-container PTY stream echoes typed input", func() bool {
		return strings.Contains(streamOutput(), marker)
	})
	t.Logf("typed input reached the in-container agent and echoed over the stream: %q", strings.TrimSpace(streamOutput()))

	// --- Preview / Snapshot / Alive reflect the in-container pane ----------------
	waitUntil(t, 10*time.Second, "Snapshot reflects the in-container pane", func() bool {
		obs, serr := as.Snapshot()
		return serr == nil && strings.Contains(obs.Content, marker)
	})
	preview, err := as.Preview(0, false)
	if err != nil {
		t.Fatalf("Preview over the wire: %v", err)
	}
	if !strings.Contains(preview, marker) {
		t.Fatalf("Preview did not reflect the typed input: %q", preview)
	}
	if !as.Alive() {
		t.Fatal("expected the in-container workspace to report Alive")
	}
	t.Logf("preview/liveness reflect the in-container workspace over the wire")

	// --- Capability matrix: the REAL docker backend services full parity --------
	// (#1592 Phase 4 PR8): descriptor parity + attach/input/preview/liveness
	// serviced over the wire. Archive/Recover are exercised live by
	// TestDockerBackendArchiveRestore.
	assertLiveCapabilityMatrix(t, "docker", inst)

	// --- Kill: tear the workspace down AND reap the container -------------------
	if err := inst.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	killed = true
	waitUntil(t, 30*time.Second, "the session's container is reaped after Kill", func() bool {
		return len(containersForLabel(t, slug)) == 0
	})
	t.Logf("container reaped on Kill — no leak. docker round-trip complete.")
}

// --- docker round-trip helpers ---------------------------------------------

// requireDocker skips the test when the docker CLI or daemon is unavailable (e.g.
// inside the test-container fence), so the full suite stays green there while the
// host `make backend-docker-roundtrip` runs it for real.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping the docker backend round-trip")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not reachable; skipping the docker backend round-trip: %s", strings.TrimSpace(string(out)))
	}
}

// buildStaticBinary builds a fully static `af` (CGO_ENABLED=0) so it runs inside
// the slim alpine (musl) test image the runtime copies it into.
func buildStaticBinary(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(file))
	bin := filepath.Join(t.TempDir(), "af")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, repoRoot)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("static go build failed: %v\n%s", err, out)
	}
	return bin
}

// buildDockerRoundTripImage builds a slim image carrying git + tmux + bash — the
// minimum a BYO image needs for the in-container agent-server (git worktree + tmux
// PTY). Returns the image tag. Cached across runs by docker's layer cache.
func buildDockerRoundTripImage(t *testing.T) string {
	t.Helper()
	const tag = "af-docker-roundtrip:test"
	dir := t.TempDir()
	dockerfile := "FROM alpine:3.20\nRUN apk add --no-cache git tmux bash\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", tag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the round-trip image failed (needs network on first run): %v\n%s", err, out)
	}
	return tag
}

// writeDockerRepoConfig drops a repo config selecting the docker backend with the
// given image and run_args (the bind-mount that serves the clone source).
func writeDockerRepoConfig(t *testing.T, repo, image string, runArgs []string) {
	t.Helper()
	dir := filepath.Join(repo, ".agent-factory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir repo config: %v", err)
	}
	quotedArgs := make([]string, len(runArgs))
	for i, a := range runArgs {
		quotedArgs[i] = fmt.Sprintf("%q", a)
	}
	body := fmt.Sprintf(`{"backend":"docker","docker":{"image":%q,"run_args":[%s]}}`,
		image, strings.Join(quotedArgs, ","))
	writeFile(t, filepath.Join(dir, "config.json"), body, 0644)
}

// containersForLabel returns the ids of containers carrying the session label.
func containersForLabel(t *testing.T, slug string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "-f", "label=af.session="+slug).CombinedOutput()
	if err != nil {
		t.Fatalf("docker ps: %v\n%s", err, out)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			ids = append(ids, s)
		}
	}
	return ids
}

// reapByLabel best-effort removes any container carrying the session label — the
// test's backstop so a container never leaks even on an early failure.
func reapByLabel(slug string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "-f", "label=af.session="+slug).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	}
}
