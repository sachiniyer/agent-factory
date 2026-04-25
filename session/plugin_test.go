package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
	tmpDir := t.TempDir()
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
