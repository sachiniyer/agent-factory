package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

type remoteHTTPEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type remoteSnapshotResponse struct {
	Instances []instanceData `json:"instances"`
}

// TestRemoteHookRoundTripMockRemote is the #1592 Phase 4 PR7 proof: the
// remote-hook backend, migrated to provision-and-expose, drives a session over a
// REAL `af agent-server` that a mock launch_cmd starts — to parity with docker/ssh.
//
// A repo is configured backend=hook with a launch_cmd shell script that clones
// repo@origin, starts an `af agent-server --listen 127.0.0.1:0` against the clone
// (on the host, no container/ssh — hook is the most direct provision-and-expose),
// and echoes that server's authed {url, token} over plain HTTP. The session is
// created through the ordinary NewInstance path, so the hook runtime runs the
// script, parses the endpoint, and hands it to the daemon-side remoteAgentServer,
// which then drives the FULL surface across the process boundary:
//
//	Start (Provision+Launch the workspace) → Subscribe to its PTY stream →
//	Input (typed bytes reach the agent) → observe the echo → Preview/Snapshot/Alive
//	reflect the pane → Kill → delete_cmd reaps the af agent-server (no leak).
//
// This is the mock-hook round-trip the plan asks for: launch_cmd echoes an
// af agent-server URL → the daemon drives it over http/ws → typed input echoes →
// teardown. Run it in the container fence: make remote-roundtrip-container. It
// needs git + tmux (the in-workspace agent-server runs the local tmux runtime).
func TestRemoteHookRoundTripMockRemote(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")
	testguard.IsolateTmux(t)

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	afBin := buildBinary(t)

	// Source repo + a bare clone the mock launch_cmd clones the workspace from
	// (GitHub is the durable store; here a local bare repo stands in — self-
	// contained, no network).
	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "hook round-trip\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", bare)

	// State dir the mock hooks provision into (clone + agent-server home + pidfile).
	state := t.TempDir()
	launch := writeMockHookLaunch(t, filepath.Join(repo, "launch.sh"), afBin, state)
	del := writeMockHookDelete(t, filepath.Join(repo, "delete.sh"), state)
	writeHookRepoConfig(t, repo, launch, del)

	title := "hook-rt"
	slug := session.Slugify(title)

	// --- create the session on the hook backend (the full NewInstance path) ----
	// program `cat` echoes the PTY, so typed input observably comes back over the
	// stream — the input-reaches-the-agent proof.
	t.Logf("provisioning hook session %q via mock launch_cmd...", title)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:       title,
		Path:        repo,
		Program:     "cat",
		ForceRemote: true,
	})
	if err != nil {
		t.Fatalf("NewInstance(backend=hook): %v", err)
	}
	// The af agent-server is up the instant NewInstance returns (launch_cmd started
	// it). Route every later failure through Kill so delete_cmd reaps it.
	if !mockHookServerAlive(state, slug) {
		t.Fatal("expected the mock launch_cmd to have started an af agent-server")
	}
	t.Logf("launch_cmd started an af agent-server; endpoint exposed over http:// with a bearer token")
	killed := false
	defer func() {
		if !killed {
			_ = inst.Kill()
		}
	}()

	// --- Start: Provision + Launch the workspace over the wire -----------------
	if err := inst.Start(true); err != nil {
		t.Fatalf("Start (drive agent-server Provision+Launch): %v", err)
	}

	as := inst.AgentServer()

	// --- Subscribe to the PTY stream across the process boundary ---------------
	sub, err := as.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe to the PTY stream: %v", err)
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

	// --- Input: typed bytes reach the agent; the `cat` pane echoes -------------
	marker := "hook-roundtrip-ping"
	if err := as.Input(0, []byte(marker+"\n")); err != nil {
		t.Fatalf("Input over the wire: %v", err)
	}
	waitUntil(t, 20*time.Second, "PTY stream echoes typed input", func() bool {
		return strings.Contains(streamOutput(), marker)
	})
	t.Logf("typed input reached the agent and echoed over the stream: %q", strings.TrimSpace(streamOutput()))

	// --- Preview / Snapshot / Alive reflect the pane ---------------------------
	waitUntil(t, 10*time.Second, "Snapshot reflects the pane", func() bool {
		obs, serr := as.Snapshot()
		return serr == nil && strings.Contains(obs.Content, marker)
	})
	preview, err := as.Preview(0, false)
	if err != nil {
		t.Fatalf("Preview over the wire: %v", err)
	}
	if !strings.Contains(preview.Content, marker) {
		t.Fatalf("Preview did not reflect the typed input: %q", preview.Content)
	}
	if alive, err := as.Alive(); err != nil || !alive {
		t.Fatal("expected the workspace to report Alive")
	}
	t.Logf("preview/liveness reflect the workspace over the wire")

	// --- Kill: tear the workspace down AND reap the af agent-server -------------
	if err := inst.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	killed = true
	waitUntil(t, 30*time.Second, "delete_cmd reaps the af agent-server after Kill", func() bool {
		return !mockHookServerAlive(state, slug)
	})
	t.Logf("af agent-server reaped by delete_cmd on Kill — no leak. hook round-trip complete.")
}

// writeMockHookLaunch writes a launch_cmd that provisions a session by cloning
// repo@origin and starting a REAL `af agent-server` against the clone, then
// echoing its authed {url, token} — the provision-and-expose
// contract. It backgrounds the server with its stdio redirected to files (so the
// launch_cmd exec returns) and records its PID for delete_cmd to reap.
func writeMockHookLaunch(t *testing.T, path, afBin, state string) string {
	t.Helper()
	body := fmt.Sprintf(`
AF_BIN=%q
STATE=%q
NAME="" TITLE="" REPO="" PROGRAM=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    --title) TITLE="$2"; shift 2;;
    --repo) REPO="$2"; shift 2;;
    --program) PROGRAM="$2"; shift 2;;
    --branch) shift 2;;
    --auto-yes) shift;;
    *) shift;;
  esac
done
[ -n "$NAME" ] || { echo "launch: --name required" >&2; exit 64; }
DIR="$STATE/$NAME"
mkdir -p "$DIR/home"
git clone -q "$REPO" "$DIR/workspace"
BANNER="$DIR/banner.json"
LOG="$DIR/agent-server.log"
: > "$BANNER"
ARGS="agent-server --listen 127.0.0.1:0 --repo $DIR/workspace --title $TITLE"
[ -n "$PROGRAM" ] && ARGS="$ARGS --program $PROGRAM"
# nohup, matching docs/remote-hooks.md's recipe exactly — this fixture is the
# doc's script, so the two must not drift (a fixture that detaches differently
# from the doc tests something no user runs). setsid was the original spelling
# and is util-linux: it does not exist on macOS, where this died with "setsid:
# command not found" and the launch reported "printed no banner", blaming af for
# a missing coreutil (#1931, #1946). nohup is POSIX and on both. The trailing &
# is what actually makes the server outlive this script; see the doc for why the
# redirects are not optional.
AGENT_FACTORY_HOME="$DIR/home" TERM=xterm nohup "$AF_BIN" $ARGS >"$BANNER" 2>"$LOG" &
echo $! > "$DIR/pid"
i=0
while [ $i -lt 200 ]; do
  grep -q '"addr"' "$BANNER" 2>/dev/null && break
  i=$((i + 1)); sleep 0.1
done
ADDR=$(sed -n 's/.*"addr":"\([^"]*\)".*/\1/p' "$BANNER")
TOKEN=$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' "$BANNER")
[ -n "$ADDR" ] || { echo "launch: af agent-server printed no banner:" >&2; cat "$LOG" >&2; exit 1; }
printf '{"url":"http://%%s","token":"%%s"}\n' "$ADDR" "$TOKEN"
`, afBin, state)
	return writeScript(t, path, body)
}

// writeMockHookDelete writes a delete_cmd that reaps the af agent-server the
// matching launch_cmd started, by PID.
func writeMockHookDelete(t *testing.T, path, state string) string {
	t.Helper()
	body := fmt.Sprintf(`
STATE=%q
NAME=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    *) shift;;
  esac
done
[ -n "$NAME" ] || { echo "delete: --name required" >&2; exit 64; }
PIDFILE="$STATE/$NAME/pid"
if [ -f "$PIDFILE" ]; then
  kill "$(cat "$PIDFILE")" 2>/dev/null || true
  rm -f "$PIDFILE"
fi
printf '{"deleted":true}\n'
`, state)
	return writeScript(t, path, body)
}

// writeHookRepoConfig drops a repo config selecting the hook backend with the
// given launch_cmd + delete_cmd (the whole provision-and-expose contract).
func writeHookRepoConfig(t *testing.T, repo, launch, del string) {
	t.Helper()
	dir := filepath.Join(repo, ".agent-factory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir repo config: %v", err)
	}
	body := fmt.Sprintf(`{"backend":"hook","remote_hooks":{"launch_cmd":%q,"delete_cmd":%q}}`, launch, del)
	writeFile(t, filepath.Join(dir, "config.json"), body, 0644)
}

// mockHookServerAlive reports whether the af agent-server the mock launch_cmd
// started for slug is still running (its PID is alive).
func mockHookServerAlive(state, slug string) bool {
	raw, err := os.ReadFile(filepath.Join(state, slug, "pid"))
	if err != nil {
		return false
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil || pid <= 0 {
		return false
	}
	return pidAlive(pid)
}

func (h *harness) startDaemon() {
	h.t.Helper()
	if pid := readDaemonPIDIfPresent(h.home); pidAlive(pid) {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, h.bin, "--daemon")
	cmd.Dir = h.repo
	cmd.Env = append(os.Environ(),
		"AGENT_FACTORY_HOME="+h.home,
		"TERM=xterm",
	)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start daemon: %v", err)
	}
	go func() { _ = cmd.Wait() }()

	h.waitHTTPHealth(10*time.Second, func() string { return stderr.String() })
}

func (h *harness) waitHTTPHealth(timeout time.Duration, stderr func() string) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, "http://unix/v1/health", nil)
		if err != nil {
			h.t.Fatalf("build health request: %v", err)
		}
		resp, err := h.httpClient().Do(req)
		if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			var snap remoteSnapshotResponse
			readyErr := h.tryHTTPPost("/v1/Snapshot", nil, &snap)
			if readyErr == nil {
				return
			}
			lastErr = readyErr
		} else {
			if err != nil {
				lastErr = err
			} else if resp != nil {
				lastErr = fmt.Errorf("health status %d", resp.StatusCode)
			}
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("daemon HTTP socket did not become healthy: lastErr=%v stderr=%s", lastErr, stderr())
}

func (h *harness) tryHTTPPost(path string, req any, dst any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal HTTP request: %w", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, "http://unix"+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("POST %s failed: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read POST %s response: %w", path, err)
	}
	var env remoteHTTPEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode POST %s envelope: %w\n%s", path, err, raw)
	}
	if resp.StatusCode != http.StatusOK || env.Error != nil {
		msg := "<nil>"
		if env.Error != nil {
			msg = env.Error.Message
		}
		return fmt.Errorf("POST %s status=%d error=%s body=%s", path, resp.StatusCode, msg, raw)
	}
	if dst != nil {
		if err := json.Unmarshal(env.Data, dst); err != nil {
			return fmt.Errorf("decode POST %s data: %w\n%s", path, err, env.Data)
		}
	}
	return nil
}

func (h *harness) httpClient() *http.Client {
	socket := filepath.Join(h.home, "daemon-http.sock")
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
	}}
}

func readDaemonPIDIfPresent(home string) int {
	raw, err := os.ReadFile(filepath.Join(home, "daemon.pid"))
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(string(raw), "%d", &pid); err != nil {
		return 0
	}
	return pid
}
