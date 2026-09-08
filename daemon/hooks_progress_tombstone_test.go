//go:build linux

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	gitworktree "github.com/sachiniyer/agent-factory/session/git"
)

func TestHookProgressTombstoneRestoreNeverResumes(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "kill_persisted", true: "archived"}[archived], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			claimDaemonProcessForRestore(t)
			shim := t.TempDir()
			for name, body := range map[string]string{
				"systemctl":   "#!/bin/sh\nexit 0\n",
				"systemd-run": "#!/bin/sh\nif [ \"$1\" = --help ]; then exit 0; fi\nwhile [ \"$1\" != -- ]; do shift; done; shift; exec \"$@\"\n",
			} {
				if err := os.WriteFile(filepath.Join(shim, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", shim+":"+os.Getenv("PATH"))
			repoPath := setupControlRepo(t)
			repo, err := config.RepoFromPath(repoPath)
			if err != nil {
				t.Fatal(err)
			}
			tree := t.TempDir()
			dir := filepath.Join(home, "logs", "hooks", "entries-tombstone")
			if err := os.MkdirAll(filepath.Join(dir, "0"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "0", "exit"), []byte("0\n"), 0600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "should-not-run")
			data, err := json.Marshal(map[string]any{"commands": []string{"true", "echo ran > " + strconv.Quote(marker)}, "worktree": tree, "session_id": "tombstone", "scope_prefix": "af-hook-tombstone", "generation": "test", "directory": dir})
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(tree))
			path := filepath.Join(home, "logs", "hooks", "progress-wt"+hex.EncodeToString(sum[:8])+".json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			live := session.LiveLost
			if archived {
				live = session.LiveArchived
			}
			if err := appendInstanceData(repo.ID, session.InstanceData{ID: "tombstone", Title: "tombstone", Path: repoPath, UserKilled: !archived, Liveness: live, StartupStateUnknown: true, BackendType: "local", Worktree: session.GitWorktreeData{RepoPath: repoPath, WorktreePath: tree, HookScopeUnitPrefix: "af-hook-tombstone"}}); err != nil {
				t.Fatal(err)
			}
			manager, err := NewManager(config.DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			instance := restoredInstanceByTitle(t, manager, "tombstone")
			if done := instance.PostWorktreeHooksDone(); done != nil {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("tombstone adopted a pending runner")
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Error("terminal session resumed repository commands")
			}
			if _, err := os.Stat(filepath.Join(dir, "finished")); err != nil {
				t.Error("terminal journal was not durably finished", err)
			}
			gw, err := gitworktree.NewGitWorktreeFromStorage(repoPath, tree, "tombstone", "", "", false, false)
			if err != nil {
				t.Fatal(err)
			}
			gw.SetHookScopeSessionID("tombstone")
			gitworktree.AdoptRunningHooks([]*gitworktree.GitWorktree{gw})
			if done := gw.HooksDone(); done != nil {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("second entry point resumed tombstone")
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Error("second entry point ran terminal session commands")
			}
		})
	}
}
