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

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

type remoteHTTPEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type remoteCreateResponse struct {
	Instance instanceData `json:"instance"`
}

type remoteSnapshotResponse struct {
	Instances []instanceData `json:"instances"`
}

func TestRemoteHookRoundTripMockRemote(t *testing.T) {
	h := newHarness(t)
	mock := newMockRemote(t, h.repo)
	h.startDaemon()

	title := "Remote Round Trip"
	slug := "remote-round-trip"

	var created remoteCreateResponse
	h.httpPost("/v1/CreateSession", daemon.CreateSessionRequest{
		Title:       title,
		RepoPath:    h.repo,
		Program:     tmux.ProgramClaude,
		ForceRemote: true,
	}, &created)
	if created.Instance.Title != title {
		t.Fatalf("create title = %q, want %q", created.Instance.Title, title)
	}
	if created.Instance.BackendType != "remote" {
		t.Fatalf("create backend = %q, want remote; response=%+v", created.Instance.BackendType, created.Instance)
	}
	if got, _ := created.Instance.RemoteMeta["name"].(string); got != slug {
		t.Fatalf("remote_meta.name = %q, want %q", got, slug)
	}
	if created.Instance.Worktree.WorktreePath != "" {
		t.Fatalf("remote create unexpectedly allocated local worktree %q", created.Instance.Worktree.WorktreePath)
	}
	assertRemoteTabs(t, created.Instance)
	mock.assertSessions(slug)
	mock.assertEvent("launch " + slug)

	var snap remoteSnapshotResponse
	h.httpPost("/v1/Snapshot", daemon.SnapshotRequest{RepoID: mock.repoID}, &snap)
	assertRemoteSnapshot(t, snap.Instances, title, slug)

	out := h.attachAndDetach(title, mock.attachCount)
	if !strings.Contains(out, "mock remote agent "+slug) {
		t.Fatalf("attach output did not contain remote stream for %q:\n%s", slug, out)
	}
	mock.assertSessions(slug)

	archiveOut, archiveErr := h.runResult("sessions", "--repo", h.repo, "archive", title)
	if archiveErr == nil {
		t.Fatalf("archive of remote session unexpectedly succeeded: %s", archiveOut)
	}
	if !strings.Contains(archiveErr.Error(), "cannot archive remote session") {
		t.Fatalf("archive error = %v, want remote rejection", archiveErr)
	}

	killPID(readDaemonPID(t, h.home))
	waitUntil(t, 5*time.Second, "daemon exits before restore check", func() bool {
		return !pidAlive(readDaemonPIDIfPresent(h.home))
	})
	h.startDaemon()

	h.httpPost("/v1/Snapshot", daemon.SnapshotRequest{RepoID: mock.repoID}, &snap)
	assertRemoteSnapshot(t, snap.Instances, title, slug)
	mock.assertEvent("list --json")

	h.run("sessions", "--repo", h.repo, "kill", title)
	waitUntil(t, 5*time.Second, "remote session deleted from mock state", func() bool {
		return len(mock.sessions()) == 0
	})
	mock.assertEvent("delete " + slug)
	waitUntil(t, 5*time.Second, "remote session removed from CLI list", func() bool {
		return !hasTitle(h.listSessions(), title)
	})
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
			readyErr := h.tryHTTPPost("/v1/Snapshot", daemon.SnapshotRequest{}, &snap)
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

func (h *harness) httpPost(path string, req any, dst any) {
	h.t.Helper()
	if err := h.tryHTTPPost(path, req, dst); err != nil {
		h.t.Fatal(err)
	}
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
	if err := json.Unmarshal(env.Data, dst); err != nil {
		return fmt.Errorf("decode POST %s data: %w\n%s", path, err, env.Data)
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

func (h *harness) attachAndDetach(title string, count func() int) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, "sessions", "--repo", h.repo, "attach", title)
	cmd.Dir = h.repo
	cmd.Env = append(os.Environ(),
		"AGENT_FACTORY_HOME="+h.home,
		"TERM=xterm",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		h.t.Fatalf("attach stdin pipe: %v", err)
	}
	var stdout lockedBuffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start attach: %v", err)
	}

	waitUntil(h.t, 5*time.Second, "interactive attach stream appears", func() bool {
		return count() > 0 && strings.Contains(stdout.String(), "mock remote agent")
	})
	if _, err := stdin.Write([]byte{tmux.DetachKeyByte}); err != nil {
		h.t.Fatalf("write detach key: %v", err)
	}
	_ = stdin.Close()

	err = cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		h.t.Fatalf("attach timed out; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if err != nil {
		h.t.Fatalf("attach failed: %v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type mockRemote struct {
	t         *testing.T
	repoID    string
	stateDir  string
	eventsLog string
}

func newMockRemote(t *testing.T, repoPath string) *mockRemote {
	t.Helper()
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	root := t.TempDir()
	stateDir := filepath.Join(root, "mock remote state")
	hooksDir := filepath.Join(repoPath, ".agent-factory", "mock remote hooks")
	if err := os.MkdirAll(filepath.Join(stateDir, "sessions"), 0755); err != nil {
		t.Fatalf("mkdir mock state: %v", err)
	}
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatalf("mkdir mock hooks: %v", err)
	}
	eventsLog := filepath.Join(stateDir, "events.log")
	writeFile(t, eventsLog, "", 0644)

	launch := writeScript(t, filepath.Join(hooksDir, "launch hook.sh"), fmt.Sprintf(`
STATE_DIR=%q
EVENTS=%q
NAME=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    --json) shift;;
    *) echo "unexpected launch arg: $1" >&2; exit 64;;
  esac
done
[ -n "$NAME" ] || { echo "missing --name" >&2; exit 64; }
mkdir -p "$STATE_DIR/sessions/$NAME"
printf 'running\n' > "$STATE_DIR/sessions/$NAME/status"
printf 'launch %%s\n' "$NAME" >> "$EVENTS"
printf '{"name":"%%s","status":"running","host":"mock-container"}\n' "$NAME"
`, stateDir, eventsLog))

	list := writeScript(t, filepath.Join(hooksDir, "list hook.sh"), fmt.Sprintf(`
STATE_DIR=%q
EVENTS=%q
printf 'list %%s\n' "$*" >> "$EVENTS"
printf '['
sep=''
for session_dir in "$STATE_DIR"/sessions/*; do
  [ -d "$session_dir" ] || continue
  name="$(basename "$session_dir")"
  status="$(cat "$session_dir/status")"
  printf '%%s{"name":"%%s","status":"%%s","host":"mock-container"}' "$sep" "$name" "$status"
  sep=','
done
printf ']\n'
`, stateDir, eventsLog))

	attach := writeScript(t, filepath.Join(hooksDir, "attach hook.sh"), fmt.Sprintf(`
STATE_DIR=%q
EVENTS=%q
NAME="${1:-}"
[ -n "$NAME" ] || { echo "missing session name" >&2; exit 64; }
[ -d "$STATE_DIR/sessions/$NAME" ] || { echo "unknown session $NAME" >&2; exit 66; }
printf 'attach %%s\n' "$NAME" >> "$EVENTS"
i=0
while :; do
  printf '\033[2J\033[Hmock remote agent %%s tick %%s\n' "$NAME" "$i"
  i=$((i + 1))
  sleep 1
done
`, stateDir, eventsLog))

	terminal := writeScript(t, filepath.Join(hooksDir, "terminal hook.sh"), fmt.Sprintf(`
STATE_DIR=%q
EVENTS=%q
NAME="${1:-}"
[ -n "$NAME" ] || { echo "missing session name" >&2; exit 64; }
[ -d "$STATE_DIR/sessions/$NAME" ] || { echo "unknown session $NAME" >&2; exit 66; }
printf 'terminal %%s\n' "$NAME" >> "$EVENTS"
printf 'mock remote terminal %%s\n' "$NAME"
`, stateDir, eventsLog))

	del := writeScript(t, filepath.Join(hooksDir, "delete hook.sh"), fmt.Sprintf(`
STATE_DIR=%q
EVENTS=%q
NAME=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    --json) shift;;
    *) echo "unexpected delete arg: $1" >&2; exit 64;;
  esac
done
[ -n "$NAME" ] || { echo "missing --name" >&2; exit 64; }
rm -rf "$STATE_DIR/sessions/$NAME"
printf 'delete %%s\n' "$NAME" >> "$EVENTS"
printf '{"name":"%%s","deleted":true}\n' "$NAME"
`, stateDir, eventsLog))

	raw, err := json.MarshalIndent(map[string]any{
		"remote_hooks": map[string]string{
			"launch_cmd":   launch,
			"list_cmd":     list,
			"attach_cmd":   attach,
			"delete_cmd":   del,
			"terminal_cmd": terminal,
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal mock remote config: %v", err)
	}
	writeFile(t, config.InRepoConfigPath(repoPath), string(raw), 0644)

	return &mockRemote{
		t:         t,
		repoID:    repo.ID,
		stateDir:  stateDir,
		eventsLog: eventsLog,
	}
}

func (m *mockRemote) sessions() []string {
	m.t.Helper()
	entries, err := os.ReadDir(filepath.Join(m.stateDir, "sessions"))
	if err != nil {
		m.t.Fatalf("read mock sessions: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

func (m *mockRemote) assertSessions(want ...string) {
	m.t.Helper()
	got := strings.Join(m.sessions(), ",")
	if got != strings.Join(want, ",") {
		m.t.Fatalf("mock sessions = %q, want %q", got, strings.Join(want, ","))
	}
}

func (m *mockRemote) assertEvent(event string) {
	m.t.Helper()
	if !strings.Contains(readFile(m.t, m.eventsLog), event) {
		m.t.Fatalf("mock event log missing %q:\n%s", event, readFile(m.t, m.eventsLog))
	}
}

func (m *mockRemote) attachCount() int {
	m.t.Helper()
	return strings.Count(readFile(m.t, m.eventsLog), "attach ")
}

func assertRemoteSnapshot(t *testing.T, instances []instanceData, title, slug string) {
	t.Helper()
	for _, inst := range instances {
		if inst.Title != title {
			continue
		}
		if inst.BackendType != "remote" {
			t.Fatalf("snapshot backend = %q, want remote; instance=%+v", inst.BackendType, inst)
		}
		if got, _ := inst.RemoteMeta["name"].(string); got != slug {
			t.Fatalf("snapshot remote_meta.name = %q, want %q", got, slug)
		}
		if inst.Worktree.WorktreePath != "" {
			t.Fatalf("snapshot remote worktree path = %q, want empty", inst.Worktree.WorktreePath)
		}
		assertRemoteTabs(t, inst)
		return
	}
	t.Fatalf("snapshot missing %q in %+v", title, instances)
}

func assertRemoteTabs(t *testing.T, inst instanceData) {
	t.Helper()
	if len(inst.Tabs) != 2 {
		t.Fatalf("remote tabs = %+v, want agent + terminal", inst.Tabs)
	}
	if inst.Tabs[0].Name != "agent" || inst.Tabs[0].Kind != 0 {
		t.Fatalf("remote agent tab = %+v, want agent kind 0", inst.Tabs[0])
	}
	if inst.Tabs[1].Name != "shell" || inst.Tabs[1].Kind != 1 {
		t.Fatalf("remote terminal tab = %+v, want shell kind 1", inst.Tabs[1])
	}
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
