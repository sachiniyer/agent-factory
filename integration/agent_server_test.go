package integration_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// agentServerBanner mirrors daemon.AgentServerInfo — the JSON startup line the
// `af agent-server` process prints to stdout the instant its listener binds.
type agentServerBanner struct {
	Addr        string `json:"addr"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
	SelfSigned  bool   `json:"self_signed"`
	CertPath    string `json:"cert_path"`
	Title       string `json:"title"`
}

// TestAgentServerRoundTrip is the #1592 Phase 4 PR1 payoff: it starts a REAL,
// out-of-process `af agent-server` on a loopback TLS+token listener against a real
// git repo + tmux session, then drives it directly over HTTPS/WSS presenting the
// token — provisioning + launching the workspace, subscribing to the PTY stream,
// typing input and seeing the pane echo it, and reading a snapshot. This proves
// the agent-server is a real out-of-process runtime a remote daemon can drive over
// the wire exactly like the in-process local one (PR2), across the process
// boundary the whole phase is built on.
//
// Run it in the container fence: make agent-server-roundtrip-container.
func TestAgentServerRoundTrip(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")
	testguard.IsolateTmux(t)

	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	// The fake agent pane runs `cat` (echoes input) behind a wrapper that prints a
	// ready prompt and swallows the agent-specific flags injectSystemPrompt adds —
	// the same harness the WS PTY broker round-trip uses.
	wrapper := home + "/fake-agent.sh"
	writeFile(t, wrapper, "#!/bin/sh\nprintf '❯ '\nexec cat\n", 0755)
	writeConfigWithProgramPath(t, home, wrapper)

	bin := buildBinary(t)
	repo := setupGitRepo(t)

	// --- start the real out-of-process agent-server -------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "agent-server",
		"--listen", "127.0.0.1:0", "--repo", repo, "--title", "probe", "--program", tmux.ProgramClaude)
	cmd.Env = append(os.Environ(), "AGENT_FACTORY_HOME="+home, "TERM=xterm")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		// Backstop: sweep the workspace tmux session if teardown did not.
		if name := tmuxNameForProbe(); name != "" && tmuxSessionExists(name) {
			_ = exec.Command("tmux", "kill-session", "-t="+name).Run()
		}
	})

	// --- read the machine-readable startup banner ---------------------------
	banner := readAgentServerBanner(t, stdout)
	if banner.Addr == "" || banner.Token == "" || banner.CertPath == "" {
		t.Fatalf("incomplete startup banner: %+v", banner)
	}
	if !strings.HasPrefix(banner.Fingerprint, "sha256:") {
		t.Fatalf("expected a sha256 cert fingerprint, got %q", banner.Fingerprint)
	}

	t.Logf("agent-server up: addr=%s self_signed=%v fingerprint=%s title=%s (token %d bytes)",
		banner.Addr, banner.SelfSigned, banner.Fingerprint, banner.Title, len(banner.Token))

	client := pinnedClient(t, banner.CertPath)
	base := "https://" + banner.Addr

	post := func(path, body string) map[string]any {
		t.Helper()
		req, rerr := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		if rerr != nil {
			t.Fatalf("new request %s: %v", path, rerr)
		}
		req.Header.Set("Authorization", "Bearer "+banner.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, rerr := client.Do(req)
		if rerr != nil {
			t.Fatalf("POST %s: %v", path, rerr)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, body)
		}
		var env struct {
			Data  map[string]any `json:"data"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if env.Error != nil {
			t.Fatalf("POST %s returned error: %s", path, env.Error.Message)
		}
		return env.Data
	}

	// --- create the session in the agent-server's runtime -------------------
	post("/v1/agent/provision", `{"first_time_setup":true}`)
	post("/v1/agent/launch", `{"first_time_setup":true}`)

	// --- subscribe to the PTY stream over WSS -------------------------------
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer wsCancel()
	conn, _, err := websocket.Dial(wsCtx,
		"wss://"+banner.Addr+"/v1/sessions/probe/stream?"+agentproto.AccessTokenQueryParam+"="+banner.Token,
		&websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatalf("dial PTY stream: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(1 << 20)

	// Accumulate PTY_OUT bytes in a read pump.
	var (
		pumpMu sync.Mutex
		pump   strings.Builder
	)
	go func() {
		for {
			msg, rerr := agentproto.ReadMessage(wsCtx, conn)
			if rerr != nil {
				return
			}
			if msg.Binary && msg.Frame.Op == agentproto.OpPTYOut {
				pumpMu.Lock()
				pump.Write(msg.Frame.Data)
				pumpMu.Unlock()
			}
		}
	}()
	output := func() string {
		pumpMu.Lock()
		defer pumpMu.Unlock()
		return pump.String()
	}

	// --- send input; the `cat` pane echoes it back over the stream ----------
	if err := agentproto.WriteFrame(wsCtx, conn, agentproto.InputFrame([]byte("hello-agent-server\n"))); err != nil {
		t.Fatalf("send input: %v", err)
	}
	waitUntil(t, 10*time.Second, "PTY stream echoes typed input", func() bool {
		return strings.Contains(output(), "hello-agent-server")
	})
	t.Logf("PTY stream echoed typed input over WSS: %q", strings.TrimSpace(output()))

	// --- read a snapshot over the control REST ------------------------------
	snap := post("/v1/agent/snapshot", ``)
	content, _ := snap["content"].(string)
	if !strings.Contains(content, "hello-agent-server") {
		t.Fatalf("snapshot content did not reflect the typed input: %q", content)
	}
	t.Logf("snapshot over control REST reflects the pane: %q", strings.TrimSpace(content))

	// --- alive reports the workspace is running -----------------------------
	alive := post("/v1/agent/alive", ``)
	if a, _ := alive["alive"].(bool); !a {
		t.Fatalf("expected the workspace to be alive, got %+v", alive)
	}
}

// readAgentServerBanner reads the first stdout line and decodes it as the JSON
// startup banner, then drains the rest of stdout so the pipe never blocks the
// child.
func readAgentServerBanner(t *testing.T, stdout io.Reader) agentServerBanner {
	t.Helper()
	reader := bufio.NewReader(stdout)
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		ch <- result{line: line, err: err}
	}()
	var line string
	select {
	case r := <-ch:
		if r.err != nil && r.line == "" {
			t.Fatalf("read startup banner: %v", r.err)
		}
		line = r.line
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the agent-server startup banner")
	}
	go func() { _, _ = io.Copy(io.Discard, reader) }()

	var banner agentServerBanner
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &banner); err != nil {
		t.Fatalf("decode startup banner %q: %v", line, err)
	}
	return banner
}

// pinnedClient builds an HTTPS client that trusts ONLY the agent-server's
// self-signed cert (TOFU pin), mirroring what a remote daemon does.
func pinnedClient(t *testing.T, certPath string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert %s: %v", certPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("load cert %s into pool", certPath)
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
}

// tmuxNameForProbe derives the sanitized tmux session name for the "probe"
// workspace so cleanup can sweep it if the agent-server's own teardown did not.
func tmuxNameForProbe() string {
	return tmux.NewTmuxSessionForRepo("probe", "", tmux.ProgramClaude).SanitizedName()
}
