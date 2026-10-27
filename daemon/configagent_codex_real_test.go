package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/configagent"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const realCodexConfigAgentSentinel = "AF_CONFIG_AGENT_2220_BRIEFING_SUBMITTED"

// TestRealCodexConfigAgentSubmitsBriefing is an opt-in receiver-level gate for
// #2220. It is not part of the hermetic suite because it needs an authenticated
// real Codex binary. Run it only in the testbox with throwaway AF and Codex
// homes; AF_REAL_CODEX_BIN names the binary mounted into that container.
//
// The assertion reads Codex's own rollout, not af's send return. A matching
// user message can only exist after Codex accepted the composer as a turn, which
// is the distinction the production failure exposed. The immediate pane capture
// is logged as the other half of the diagnosis: on the failure it shows whether
// the briefing is still sitting in the composer after Spawn reported success.
func TestRealCodexConfigAgentSubmitsBriefing(t *testing.T) {
	env := newRealCodexConfigAgentEnv(t)
	prompt := configagent.BuildBriefing(
		configagent.ModeOnboard,
		env.cfg,
		filepath.Join(env.afHome, config.ConfigFileName),
	) + "\n\n" + realCodexConfigAgentSentinel

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sessionName, _, err := env.manager.SpawnConfigAgent(ctx, daemon.SpawnConfigAgentRequest{
		Program: env.codexBin,
		Prompt:  prompt,
	})
	if err != nil {
		t.Fatalf("spawn real Codex config agent: %v", err)
	}
	t.Cleanup(func() {
		if err := env.manager.ReapConfigAgent(daemon.ReapConfigAgentRequest{SessionName: sessionName}); err != nil {
			t.Errorf("reap real Codex config agent: %v", err)
		}
	})

	immediate := captureConfigAgentPane(t, sessionName)
	t.Logf("pane immediately after Spawn returned:\n%s", immediate)

	rollouts := filepath.Join(env.codexHome, "sessions")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		found, scanErr := treeContains(rollouts, realCodexConfigAgentSentinel)
		if scanErr != nil {
			t.Fatalf("scan Codex rollouts: %v", scanErr)
		}
		if found {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("Spawn returned success but Codex recorded no user turn containing the briefing sentinel; final pane:\n%s",
		captureConfigAgentPane(t, sessionName))
}

// TestRealCodexConfigAgentFullBriefingReceipt verifies the receiver-side receipt
// for the actual production input shape: the UNMODIFIED configagent.BuildBriefing
// output, which ends with trailing whitespace ("\n\n"). Codex 0.144.6 trims a
// submitted turn before recording it (chat_composer.rs
// prepare_submission_text_with_options: text.trim().to_string()), so the receipt
// matcher must trim the prompt's trailing whitespace to confirm the recorded turn.
// Without that (#5488) every real spawn logged "briefing receipt was not observed"
// even though the briefing landed. This gate asserts a positive receipt via
// session.WaitForPromptReceipt against the full, unmodified briefing — the
// assertion the original receipt feature (ef6c955e) claimed but never shipped,
// because the existing sentinel test pads the prompt so it never ends in
// whitespace and so never exercises the trim.
func TestRealCodexConfigAgentFullBriefingReceipt(t *testing.T) {
	env := newRealCodexConfigAgentEnv(t)

	// Snapshot the Codex store BEFORE spawn so WaitForPromptReceipt only
	// considers the rollout this spawn creates.
	receiptSnapshot := session.BeginConversationCaptureAtCodexHome(env.codexHome)

	briefing := configagent.BuildBriefing(
		configagent.ModeOnboard,
		env.cfg,
		filepath.Join(env.afHome, config.ConfigFileName),
	)
	// The briefing ends with trailing whitespace ("\n\n"); do NOT trim or pad
	// it. The receipt must confirm this exact production shape.

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sessionName, _, err := env.manager.SpawnConfigAgent(ctx, daemon.SpawnConfigAgentRequest{
		Program: env.codexBin,
		Prompt:  briefing,
	})
	if err != nil {
		t.Fatalf("spawn real Codex config agent: %v", err)
	}
	t.Cleanup(func() {
		if err := env.manager.ReapConfigAgent(daemon.ReapConfigAgentRequest{SessionName: sessionName}); err != nil {
			t.Errorf("reap real Codex config agent: %v", err)
		}
	})

	// Directly confirm the full, unmodified briefing is recorded as a Codex user
	// turn. This is the positive receiver-side signal the spawn's internal wait
	// also relies on; calling it here with the verbatim BuildBriefing output
	// keeps the assertion honest for the trailing-whitespace shape that
	// previously never confirmed.
	if err := session.WaitForPromptReceipt(ctx, tmux.ProgramCodex, receiptSnapshot, briefing, 30*time.Second); err != nil {
		t.Fatalf("the full BuildBriefing output (ending in trailing whitespace) must be confirmed as a Codex user turn: %v", err)
	}
}

// newRealCodexConfigAgentEnv performs the throwaway-home setup shared by the
// opt-in real-Codex config-agent tests. It skips when
// AF_REAL_CODEX_CONFIG_AGENT is not set, so the suite is inert outside the
// isolated Codex testbox.
func newRealCodexConfigAgentEnv(t *testing.T) *realCodexConfigAgentEnv {
	t.Helper()
	if os.Getenv("AF_REAL_CODEX_CONFIG_AGENT") != "1" {
		t.Skip("set AF_REAL_CODEX_CONFIG_AGENT=1 inside the isolated Codex testbox")
	}
	codexBin := strings.TrimSpace(os.Getenv("AF_REAL_CODEX_BIN"))
	if codexBin == "" {
		t.Fatal("AF_REAL_CODEX_BIN must name the real Codex binary in the container")
	}
	if _, err := os.Stat(codexBin); err != nil {
		t.Fatalf("real Codex binary: %v", err)
	}
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		t.Fatal("CODEX_HOME must be a throwaway directory in the container")
	}

	testguard.IsolateTmux(t)
	afHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	if os.Getenv("AF_REAL_CODEX_PRETRUST") == "1" {
		trustedProject := fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"\n", afHome)
		if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(trustedProject), 0600); err != nil {
			t.Fatalf("pre-trust disposable AF home: %v", err)
		}
	}

	cfg := config.DefaultConfig()
	manager, err := daemon.NewManager(cfg)
	if err != nil {
		t.Fatalf("construct manager: %v", err)
	}
	return &realCodexConfigAgentEnv{
		manager:   manager,
		cfg:       cfg,
		codexHome: codexHome,
		afHome:    afHome,
		codexBin:  codexBin,
	}
}

type realCodexConfigAgentEnv struct {
	manager   *daemon.Manager
	cfg       *config.Config
	codexHome string
	afHome    string
	codexBin  string
}

func captureConfigAgentPane(t *testing.T, sessionName string) string {
	t.Helper()
	cmd := exec.Command("tmux", "capture-pane", "-p", "-J", "-S", "-", "-t", fmt.Sprintf("=%s:", sessionName))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("capture config-agent pane: %v: %s", err, out)
	}
	return string(out)
}

func treeContains(root, needle string) (bool, error) {
	found := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || found {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found = strings.Contains(string(data), needle)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return found, err
}
