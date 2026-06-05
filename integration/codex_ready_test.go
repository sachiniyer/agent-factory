package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestCLICreateCodexSessionBecomesReady is the regression test for
// sachiniyer/agent-factory#714. Before isReadyContent became agent-aware, a
// codex session's "›" (U+203A) prompt glyph never matched the claude-only
// ready check, so `af sessions create --program codex` blocked inside
// waitForReady for the full 60s timeout (well past the CLI's 30s deadline) and
// failed. The empty-prompt early-return removed in #709 is what newly routed
// every create through waitForReady and exposed the blind spot.
//
// The fake codex wrapper prints codex's banner and the "›" prompt, then reads
// stdin — mirroring the claude fixture in newHarness. With the agent-aware fix
// the create returns promptly; without it this create times out and errors.
func TestCLICreateCodexSessionBecomesReady(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")

	home := t.TempDir()
	repo := setupGitRepo(t)

	// printf interprets the \n (backslash-n) into a newline; the "›" is the
	// literal U+203A glyph. The wrapper ignores any injected flags
	// (-c developer_instructions=…) exactly like the claude fixture.
	wrapper := filepath.Join(home, "fake-codex.sh")
	writeFile(t, wrapper, "#!/bin/sh\nprintf 'OpenAI Codex (vX)\\npermissions: YOLO mode\\n› '\nexec cat\n", 0755)

	cfg := testConfig()
	cfg.DefaultProgram = tmux.ProgramCodex
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: wrapper}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	writeFile(t, filepath.Join(home, config.ConfigFileName), string(raw), 0644)

	bin := buildBinary(t)
	// Capture stdout separately from stderr: the CLI writes the session JSON
	// to stdout but a "wrote logs to …" line to stderr, so CombinedOutput
	// would corrupt the JSON parse.
	runAF := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "AGENT_FACTORY_HOME="+home, "TERM=xterm")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			return stdout.String(), fmt.Errorf("timed out; stderr=%s", stderr.String())
		}
		if err != nil {
			return stdout.String(), fmt.Errorf("%w; stderr=%s", err, stderr.String())
		}
		return stdout.String(), nil
	}

	t.Cleanup(func() {
		_, _ = runAF("sessions", "kill", "codex-ready")
		killDaemonFromHome(home)
	})

	start := time.Now()
	out, err := runAF("sessions", "--repo", repo, "create", "--name", "codex-ready", "--program", tmux.ProgramCodex)
	if err != nil {
		t.Fatalf("create codex session failed (regression #714: codex prompt not recognized as ready): %v\n%s", err, out)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("codex create took %s; waitForReady likely did not recognize the codex prompt", elapsed)
	}

	var data instanceData
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("parse create response: %v\n%s", err, out)
	}
	if data.Title != "codex-ready" {
		t.Fatalf("unexpected create response: %+v", data)
	}
}

// TestCLICreateCodexWaitsPastTrustPrompt is the regression test for
// sachiniyer/agent-factory#729. The #714/#715 fix added "Do you trust this
// folder" to codex's ready signals so waitForReady would exit on the trust
// dialog — but codex has no trust-dismissal in CheckAndHandleTrustPrompt, so
// the next user prompt was typed into the dialog instead of the agent.
//
// The fix removes the trust string from codex's ready set: a codex pane
// showing only the trust dialog is NOT ready, and waitForReady must keep
// waiting until the real "›" prompt appears. The fake codex wrapper prints
// the trust dialog, holds it for a few seconds, then prints the "›" prompt —
// mirroring codex resolving the dialog before becoming ready.
//
// Discriminator: with the bug, waitForReady exits on the trust dialog and the
// create returns almost immediately; with the fix it must wait for the "›"
// prompt, so the create cannot complete before the wrapper emits it.
func TestCLICreateCodexWaitsPastTrustPrompt(t *testing.T) {
	requireTool(t, "git")
	requireTool(t, "tmux")

	home := t.TempDir()
	repo := setupGitRepo(t)

	// The wrapper prints codex's workspace-trust dialog (no "›"), holds it for
	// trustHold, then prints the "›" prompt and reads stdin. The trust dialog
	// alone must not satisfy waitForReady; only the trailing "›" does. The
	// hold is set well above session-startup overhead (daemon launch + git
	// worktree, observed ~6s) so the timing assertion below is unambiguous.
	const trustHold = 12 * time.Second
	wrapper := filepath.Join(home, "fake-codex-trust.sh")
	writeFile(t, wrapper,
		"#!/bin/sh\n"+
			"printf 'OpenAI Codex (vX)\\nDo you trust this folder?\\n> 1. Yes\\n'\n"+
			"sleep 12\n"+
			"printf '\\342\\200\\272 '\n"+ // U+203A "›" in octal-escaped UTF-8
			"exec cat\n",
		0755)

	cfg := testConfig()
	cfg.DefaultProgram = tmux.ProgramCodex
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: wrapper}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	writeFile(t, filepath.Join(home, config.ConfigFileName), string(raw), 0644)

	bin := buildBinary(t)
	runAF := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "AGENT_FACTORY_HOME="+home, "TERM=xterm")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			return stdout.String(), fmt.Errorf("timed out; stderr=%s", stderr.String())
		}
		if err != nil {
			return stdout.String(), fmt.Errorf("%w; stderr=%s", err, stderr.String())
		}
		return stdout.String(), nil
	}

	t.Cleanup(func() {
		_, _ = runAF("sessions", "kill", "codex-trust")
		killDaemonFromHome(home)
	})

	start := time.Now()
	out, err := runAF("sessions", "--repo", repo, "create", "--name", "codex-trust", "--program", tmux.ProgramCodex)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("create codex session failed: %v\n%s", err, out)
	}

	// With the #729 regression, the trust dialog counted as ready and the
	// create returned at startup time (~6s) — before the wrapper emitted the
	// "›" prompt at trustHold. The fix makes waitForReady block past the trust
	// dialog, so the create cannot finish before the prompt appears. The
	// threshold sits comfortably above startup overhead and below trustHold.
	const minElapsed = trustHold - 3*time.Second
	if elapsed < minElapsed {
		t.Fatalf("create returned in %s (< %s), before the codex prompt appeared at ~%s: the trust dialog was treated as ready (#729 regression)", elapsed, minElapsed, trustHold)
	}

	var data instanceData
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("parse create response: %v\n%s", err, out)
	}
	if data.Title != "codex-trust" {
		t.Fatalf("unexpected create response: %+v", data)
	}
}
