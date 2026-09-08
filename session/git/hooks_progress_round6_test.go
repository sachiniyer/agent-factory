//go:build linux

package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookProgressUncommittedOrphanPruning(t *testing.T) {
	for _, kind := range []string{"old", "young", "owned", "archived", "live", "young-receipt"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			installSurvivorSystemctl(t, "exit 0\n")
			if kind == "owned" || kind == "archived" {
				row := `[{"id":"owner"}]`
				if kind == "archived" {
					row = `[{"id":"owner","liveness":5}]`
				}
				if err := config.SaveRepoInstances("test", json.RawMessage(row)); err != nil {
					t.Fatal(err)
				}
			}
			p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p.receipt(0), 0700); err != nil {
				t.Fatal(err)
			}
			path, _ := hookProgressPath(p.Worktree)
			if kind != "young" {
				old := time.Now().Add(-time.Minute)
				for _, file := range []string{path, p.Directory, p.receipt(0)} {
					if err := os.Chtimes(file, old, old); err != nil {
						t.Fatal(err)
					}
				}
			}
			if kind == "young-receipt" {
				now := time.Now()
				if err := os.Chtimes(p.receipt(0), now, now); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "live" {
				installSurvivorSystemctl(t, "echo 'af-hook-owner-test-0.scope loaded active running Hook'\nexit 0\n")
			}
			pruneHookProgress(filepath.Dir(path), time.Now())
			for _, file := range []string{path, p.Directory} {
				_, err := os.Stat(file)
				if kind == "old" {
					if !os.IsNotExist(err) {
						t.Errorf("old ownerless unfinished journal retained: %s", file)
					}
				} else if err != nil {
					t.Errorf("protected %s journal removed: %s: %v", kind, file, err)
				}
			}
		})
	}
}

func TestHookProgressAdoptsAcrossHomeAlias(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	canonical := t.TempDir()
	alias := filepath.Join(t.TempDir(), "home-alias")
	if err := os.Symlink(canonical, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_FACTORY_HOME", alias)
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath = linkedHookWorktree(t)
	g.branchName = "hook-resume"
	marker := filepath.Join(g.worktreePath, "ran")
	p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"true", "echo second >> " + shellQuoteForShim(marker)}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.receipt(0), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.receipt(0), "exit"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_FACTORY_HOME", canonical)
	t.Cleanup(func() {
		g.hooksCancel()
		if g.HooksDone() != nil {
			waitForClosed(t, g.HooksDone(), 5*time.Second, "watcher did not stop")
		}
	})
	AdoptRunningHooks([]*GitWorktree{g})
	if g.HooksDone() == nil {
		t.Fatal("canonical home failed to adopt the aliased journal")
	}
	waitForClosed(t, g.HooksDone(), 5*time.Second, "aliased journal did not finish")
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "second\n" {
		t.Fatalf("remaining command = %q, err=%v", raw, err)
	}
	loaded, _, err := g.ownedHookProgress()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Directory != filepath.Join(canonical, "logs", "hooks", filepath.Base(p.Directory)) || !loaded.completed() {
		t.Fatal("adopted receipts were not rebased and completed")
	}
}

func TestHookProgressRefusesForeignReceiptDirectory(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := hookProgressPath(p.Worktree)
	// Even a same-basename directory elsewhere must not redirect the journal;
	// the valid local directory deliberately remains present for this test.
	p.Directory = filepath.Join(t.TempDir(), filepath.Base(p.Directory))
	if err := os.Mkdir(p.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readHookProgress(path); err == nil {
		t.Fatal("foreign receipt directory was accepted")
	}
}
