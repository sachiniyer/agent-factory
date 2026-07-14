package integration_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestRemoteAgentServerRoundTrip is the #1592 Phase 4 PR2 de-risk payoff: it
// starts a REAL, out-of-process `af agent-server` (PR1) on a loopback HTTP+token
// listener, constructs a daemon-side session.remoteAgentServer pointing at it, and
// drives the FULL AgentServer surface across the process boundary — Provision +
// Launch the workspace in the remote runtime, Subscribe to its PTY stream, Input
// and see the pane echo it, Snapshot + Preview reflecting the pane, Alive, and
// Kill — exactly as the daemon drives the in-process local runtime.
//
// This proves the hard part of the whole phase — the daemon driving a sandboxed
// agent-server over the wire, behaviorally indistinguishable from local — before
// any docker/ssh runtime adds sandbox provisioning on top (PR4/PR5).
//
// Run it in the container fence: make remote-agent-server-roundtrip-container.
func TestRemoteAgentServerRoundTrip(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")
	testguard.IsolateTmux(t)

	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	// The fake agent pane runs `cat` (echoes input) behind a wrapper that prints a
	// ready prompt and swallows the agent-specific flags injectSystemPrompt adds.
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

	banner := readAgentServerBanner(t, stdout)
	if banner.Addr == "" || banner.Token == "" {
		t.Fatalf("incomplete startup banner: %+v", banner)
	}
	t.Logf("agent-server up: addr=%s title=%s (token %d bytes)",
		banner.Addr, banner.Title, len(banner.Token))

	// --- construct the daemon-side remote agent-server ----------------------
	// This is the exact impl Instance.AgentServer() returns for a remote-runtime
	// session; the whole point is that it satisfies session.AgentServer identically
	// to the in-process local one, so the daemon above it is unchanged.
	as, err := session.NewRemoteAgentServer(session.AgentServerEndpoint{
		URL:   "http://" + banner.Addr,
		Token: banner.Token,
	}, "probe")
	if err != nil {
		t.Fatalf("NewRemoteAgentServer: %v", err)
	}

	// --- create the session in the remote runtime (control REST) ------------
	if err := as.Provision(true); err != nil {
		t.Fatalf("Provision over the wire: %v", err)
	}
	if err := as.Launch(true); err != nil {
		t.Fatalf("Launch over the wire: %v", err)
	}

	// --- Subscribe to the PTY stream across the process boundary ------------
	sub, err := as.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe to remote PTY stream: %v", err)
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
			// The broker paints a fresh subscriber (PTYRepaint) then streams live
			// output (PTYData); both carry pane text through the daemon-side fan-out.
			if ev.Kind == session.PTYData || ev.Kind == session.PTYRepaint {
				pumpMu.Lock()
				pump.Write(ev.Data)
				pumpMu.Unlock()
			}
		}
	}()
	output := func() string {
		pumpMu.Lock()
		defer pumpMu.Unlock()
		return pump.String()
	}

	// --- Input; the remote `cat` pane echoes it back over the stream --------
	if err := as.Input(0, []byte("hello-remote\n")); err != nil {
		t.Fatalf("Input over the wire: %v", err)
	}
	waitUntil(t, 10*time.Second, "remote PTY stream echoes typed input", func() bool {
		return strings.Contains(output(), "hello-remote")
	})
	t.Logf("remote PTY stream echoed typed input across the process boundary: %q", strings.TrimSpace(output()))

	// --- Snapshot over the control REST reflects the pane -------------------
	waitUntil(t, 10*time.Second, "remote Snapshot reflects the typed input", func() bool {
		obs, serr := as.Snapshot()
		return serr == nil && strings.Contains(obs.Content, "hello-remote")
	})
	obs, err := as.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot over the wire: %v", err)
	}
	t.Logf("Snapshot over control REST reflects the pane: %q", strings.TrimSpace(obs.Content))

	// --- Preview mirrors the pane content -----------------------------------
	preview, err := as.Preview(0, false)
	if err != nil {
		t.Fatalf("Preview over the wire: %v", err)
	}
	if !strings.Contains(preview, "hello-remote") {
		t.Fatalf("Preview did not reflect the typed input: %q", preview)
	}

	// --- Alive reports the remote workspace is running ----------------------
	if !as.Alive() {
		t.Fatal("expected the remote workspace to report Alive")
	}

	// --- Kill tears the remote workspace down; the stream ends --------------
	if err := as.Kill(); err != nil {
		t.Fatalf("Kill over the wire: %v", err)
	}
	waitUntil(t, 10*time.Second, "remote workspace reports not-Alive after Kill", func() bool {
		return !as.Alive()
	})
}
