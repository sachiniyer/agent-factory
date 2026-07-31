package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

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

// TestEnsurePluginDir_DisarmsStaleGuardWithoutBreakingLiveHook is the upgrade-
// safety lock for #2608. Claude Code loads hooks.json once, so removing the
// stale file disarms new sessions but an already-running session keeps invoking
// the exact legacy wrapper below. The removed guard must remain as an executable
// no-op until those sessions end.
func TestEnsurePluginDir_DisarmsStaleGuardWithoutBreakingLiveHook(t *testing.T) {
	tmpDir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	// Seed the two files v1.0.209 wrote before #2563 removed the guard.
	hooksDir := filepath.Join(tmpDir, "plugin", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatalf("seed hooks dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "hooks.json"), []byte(`{"hooks":{"PreToolUse":[]}}`), 0644); err != nil {
		t.Fatalf("seed hooks.json: %v", err)
	}
	legacyGuard := "#!/bin/sh\n" +
		"guard_binary=/removed/v1.0.209/af\n" +
		"if [ ! -x \"$guard_binary\" ]; then\n" +
		"  echo 'Agent Factory tmux safety guard binary is unavailable; refusing the Bash command.' >&2\n" +
		"  exit 2\n" +
		"fi\n" +
		"\"$guard_binary\" hook-guard-tmux\n" +
		"status=$?\n" +
		"if [ \"$status\" -ne 0 ]; then\n" +
		"  echo 'Agent Factory tmux safety guard failed; refusing the Bash command.' >&2\n" +
		"  exit 2\n" +
		"fi\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "guard-tmux.sh"), []byte(legacyGuard), 0755); err != nil {
		t.Fatalf("seed guard-tmux.sh: %v", err)
	}

	pluginDir, err := ensurePluginDir()
	if err != nil {
		t.Fatalf("ensurePluginDir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(hooksDir, "hooks.json")); !os.IsNotExist(err) {
		t.Errorf("stale hooks.json survived upgrade (err=%v); a new session would register the removed guard", err)
	}

	const legacyWrapper = `if [ ! -x "${CLAUDE_PLUGIN_ROOT}/hooks/guard-tmux.sh" ]; then
  echo 'Agent Factory tmux safety guard is unavailable; refusing the Bash command.' >&2; exit 2; fi;
  "${CLAUDE_PLUGIN_ROOT}/hooks/guard-tmux.sh"`
	wrapper := exec.Command("sh", "-c", legacyWrapper)
	wrapper.Env = append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+pluginDir)
	output, err := wrapper.CombinedOutput()
	if err != nil {
		t.Fatalf("live v1.0.209 hook denied Bash after upgrade: %v\n%s", err, strings.TrimSpace(string(output)))
	}
}
