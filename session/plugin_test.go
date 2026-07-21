package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestInjectedClaudePluginGuardsBroadTmuxKills starts at injectSystemPrompt,
// the production launch seam, then runs the handler Claude discovers through
// that injected plugin. A unit test of the guard alone would stay green if the
// plugin stopped installing it and would not protect a real af session (#2175).
func TestInjectedClaudePluginGuardsBroadTmuxKills(t *testing.T) {
	afHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	launch := injectSystemPrompt("claude")
	if !strings.Contains(launch, "--plugin-dir") {
		t.Fatalf("production Claude launch is missing plugin injection: %q", launch)
	}

	type hookHandler struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type hookGroup struct {
		Matcher string        `json:"matcher"`
		Hooks   []hookHandler `json:"hooks"`
	}
	var cfg struct {
		Hooks map[string][]hookGroup `json:"hooks"`
	}
	hooksPath := filepath.Join(afHome, "plugin", "hooks", "hooks.json")
	raw, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("injected Claude plugin did not install its PreToolUse hook: %v", err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse generated hook config: %v", err)
	}

	var handler hookHandler
	for _, group := range cfg.Hooks["PreToolUse"] {
		if group.Matcher == "Bash" && len(group.Hooks) == 1 {
			handler = group.Hooks[0]
			break
		}
	}
	if handler.Type != "command" || handler.Command == "" {
		t.Fatalf("generated plugin has no Bash PreToolUse command handler: %s", raw)
	}

	runHook := func(shellCommand string) string {
		t.Helper()
		input, err := json.Marshal(map[string]any{
			"hook_event_name": "PreToolUse",
			"tool_name":       "Bash",
			"tool_input": map[string]any{
				"command": shellCommand,
			},
		})
		if err != nil {
			t.Fatalf("marshal hook input: %v", err)
		}

		cmd := exec.Command("sh", "-c", handler.Command)
		cmd.Env = append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+filepath.Join(afHome, "plugin"))
		cmd.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("configured PreToolUse handler failed for %q: %v\nstderr: %s", shellCommand, err, stderr.String())
		}
		return strings.TrimSpace(stdout.String())
	}

	blocked := runHook("tmux kill-server")
	var decision struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(blocked), &decision); err != nil {
		t.Fatalf("bare kill-server did not return a structured denial: %q (%v)", blocked, err)
	}
	if decision.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("bare kill-server must be denied, got: %s", blocked)
	}

	if allowed := runHook("tmux -L af-test-guard kill-server"); allowed != "" {
		t.Fatalf("socket-scoped kill-server must be allowed, got: %s", allowed)
	}

	unproven := runHook(`"$command" safe`)
	if err := json.Unmarshal([]byte(unproven), &decision); err != nil {
		t.Fatalf("unprovable shell syntax did not return a structured denial: %q (%v)", unproven, err)
	}
	if decision.HookSpecificOutput.PermissionDecision != "deny" ||
		!strings.Contains(decision.HookSpecificOutput.PermissionDecisionReason, "literal simple commands") {
		t.Fatalf("unprovable shell syntax must fail closed with an actionable rewrite, got: %s", unproven)
	}
}

// TestEnsurePluginDir_ConcurrentStalePrune is a regression test for issues
// #321 / #343: when two sessions start at the same time, both ReadDir the
// commands directory, both decide the same .md file is stale, and one of
// the os.Remove calls returns os.ErrNotExist because the other goroutine
// already deleted it. The previous code propagated that error and silently
// disabled --plugin-dir for the affected session. With the fix
// (!os.IsNotExist guard), every concurrent caller succeeds even when stale
// files race to be pruned.
func TestEnsurePluginDir_ConcurrentStalePrune(t *testing.T) {
	tmpDir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	// Seed many stale .md files so concurrent prune calls reliably collide
	// on at least one file (without the fix, ENOENT would surface as a
	// fatal error in at least one goroutine).
	commandsDir := filepath.Join(tmpDir, "plugin", "commands")
	if err := os.MkdirAll(commandsDir, 0755); err != nil {
		t.Fatalf("failed to mkdir commands dir: %v", err)
	}
	const staleCount = 50
	for i := 0; i < staleCount; i++ {
		path := filepath.Join(commandsDir, fmt.Sprintf("stale-%d.md", i))
		if err := os.WriteFile(path, []byte("stale"), 0644); err != nil {
			t.Fatalf("failed to seed stale file: %v", err)
		}
	}

	const workers = 20
	var wg sync.WaitGroup
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, errs[i] = ensurePluginDir()
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: ensurePluginDir must tolerate ENOENT during stale-file prune: %v", i, err)
		}
	}

	// All stale files must have been pruned.
	for i := 0; i < staleCount; i++ {
		stale := filepath.Join(commandsDir, fmt.Sprintf("stale-%d.md", i))
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("expected stale file %s to be pruned, got err=%v", stale, err)
		}
	}
	// Declared command files must exist.
	for name := range pluginCommands {
		if _, err := os.Stat(filepath.Join(commandsDir, name)); err != nil {
			t.Errorf("expected %s to exist after concurrent prune: %v", name, err)
		}
	}
}
