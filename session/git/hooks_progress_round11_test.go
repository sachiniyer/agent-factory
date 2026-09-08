//go:build linux

package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHookProgressUnclaimedLauncherLeavesJournalResumable(t *testing.T) {
	claimDaemonProcess(t)
	logPath := installScopeShim(t)
	shimDir := filepath.Dir(logPath)
	gate := filepath.Join(t.TempDir(), "launcher-used")
	launcher := "#!/bin/sh\nif [ \"${1:-}\" = \"--help\" ]; then printf '%s\\n' '    --expand-environment=BOOL'; exit 0; fi\nif [ ! -f '" + gate + "' ]; then touch '" + gate + "'; exit 0; fi\nwhile [ \"$#\" -gt 0 ]; do case \"$1\" in --user|--scope|--quiet|--collect|--expand-environment=no|--unit=*|--property=*) shift ;; --) shift; break ;; *) break ;; esac; done\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "systemd-run"), []byte(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	repo := freshRepoConfig(t, []string{"true"})
	originalStop := stopHookScopeUnits
	stopHookScopeUnits = func(...string) error { return errors.New("manager unavailable") }
	t.Cleanup(func() { stopHookScopeUnits = originalStop })
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	done := runPostWorktreeHooks(t.Context(), hookRun{repoPath: repo, worktreePath: tree, progress: p})
	waitForClosed(t, done, 5*time.Second, "launcher failure did not finish runner")
	if p.finished() {
		t.Fatal("unclaimed launcher failure marked the journal finished")
	}
	if _, err := readPendingHookProgress(tree, "owner"); err != nil {
		t.Fatalf("unfinished journal was not resumable: %v", err)
	}

	stopHookScopeUnits = originalStop
	resumed := runPostWorktreeHooks(t.Context(), hookRun{worktreePath: tree, progress: p})
	waitForClosed(t, resumed, 5*time.Second, "resumed hook did not finish")
	if !p.claimed(0) || !p.finished() {
		t.Fatal("resume did not claim the unclaimed entry and finish the journal")
	}
}
