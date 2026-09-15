//go:build linux

package git

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

type hookFailureWriter func([]byte) (int, error)

func (f hookFailureWriter) Write(b []byte) (int, error) { return f(b) }

func TestHookProgressFailedLaunchNeverResumesBeforeLaterEntry(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath = linkedHookWorktree(t)
	g.branchName = "hook-resume"
	first, second := filepath.Join(g.worktreePath, "first"), filepath.Join(g.worktreePath, "second")
	p, err := newHookProgress(hookRun{repoPath: g.repoPath, worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"echo first >> " + shellQuoteForShim(first), "echo second >> " + shellQuoteForShim(second)}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	// Force cmd.Start's chdir to fail once; restore the checkout when that error
	// is reported, so the existing continue-on-error behavior can launch entry 2.
	moved := g.worktreePath + "-moved"
	if err := os.Rename(g.worktreePath, moved); err != nil {
		t.Fatal(err)
	}
	writer := log.ErrorLog.Writer()
	var restoreErr error
	log.ErrorLog.SetOutput(hookFailureWriter(func(b []byte) (int, error) {
		if strings.Contains(string(b), "failed to start") {
			restoreErr = os.Rename(moved, g.worktreePath)
		}
		return len(b), nil
	}))
	t.Cleanup(func() { log.ErrorLog.SetOutput(writer) })
	<-runPostWorktreeHooks(context.Background(), hookRun{repoPath: g.repoPath, worktreePath: g.worktreePath, progress: p})
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	log.ErrorLog.SetOutput(io.Discard)
	if raw, err := os.ReadFile(second); err != nil || string(raw) != "second\n" {
		t.Fatalf("entry 2 did not run once: %q %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(p.receipt(0), "exit")); err != nil {
		t.Errorf("failed launch has no terminal receipt: %v", err)
	}
	if err := os.Remove(filepath.Join(p.Directory, "finished")); err != nil {
		t.Fatal(err)
	}
	installSurvivorSystemctl(t, "exit 0\n")
	t.Cleanup(func() {
		g.hooksCancel()
		if g.HooksDone() != nil {
			waitForClosed(t, g.HooksDone(), 5*time.Second, "watcher did not stop")
		}
	})
	if !g.adoptHookProgress() {
		t.Fatal("did not adopt unfinished journal")
	}
	waitForClosed(t, g.HooksDone(), 5*time.Second, "resume did not finish")
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Error("failed entry 1 ran after entry 2 on resume")
	}
	if raw, err := os.ReadFile(second); err != nil || string(raw) != "second\n" {
		t.Errorf("entry 2 replayed: %q %v", raw, err)
	}
}

func TestHookProgressTerminalOrphansWithoutExitArePruned(t *testing.T) {
	for _, owner := range []string{"deleted", "archived", "archived-legacy", "tombstoned", "live", "scope-live"} {
		t.Run(owner, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			installSurvivorSystemctl(t, "exit 0\n")
			rows := `[]`
			if owner == "archived" {
				rows = `[{"id":"owner","liveness":5}]`
			}
			if owner == "archived-legacy" {
				rows = `[{"id":"owner","status":6}]`
			}
			if owner == "tombstoned" {
				rows = `[{"id":"owner","user_killed":true}]`
			}
			if owner == "live" {
				rows = `[{"id":"owner"}]`
			}
			if err := config.SaveRepoInstances("test", json.RawMessage(rows)); err != nil {
				t.Fatal(err)
			}
			p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p.receipt(0), 0700); err != nil {
				t.Fatal(err)
			}
			p.finish()
			if owner == "scope-live" {
				installSurvivorSystemctl(t, "echo 'af-hook-owner-test-0.scope loaded active running Hook'\nexit 0\n")
			}
			path, _ := hookProgressPath(p.Worktree)
			pruneHookProgress(filepath.Dir(path), time.Now())
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("journal removed within grace: %v", err)
			}
			pruneHookProgress(filepath.Dir(path), time.Now().Add(2*progressGraceAge))
			_, err = os.Stat(path)
			keep := owner == "live" || owner == "scope-live"
			if keep && err != nil {
				t.Errorf("protected journal removed: %v", err)
			}
			if !keep && !os.IsNotExist(err) {
				t.Error("terminal journal without exit leaked")
			}
		})
	}
}
