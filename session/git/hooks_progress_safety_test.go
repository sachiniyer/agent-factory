//go:build linux

package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookProgressExternalCannotAdoptOrStopOwner(t *testing.T) {
	fastHookAdoptionPoll(t)
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	owner := worktreeWithRecordedScope(t, "af-hook-owner")
	owner.SetHookScopeSessionID("owner")
	owner.repoPath, owner.worktreePath = linkedHookWorktree(t)
	owner.branchName = "hook-resume"
	_, err := newHookProgress(hookRun{repoPath: owner.repoPath, worktreePath: owner.worktreePath, scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(t.TempDir(), "release")
	managerLog := installSurvivorSystemctl(t, "if [ -f "+shellQuoteForShim(release)+" ]; then exit 0; fi\ncase \"$*\" in *list-units*) echo 'af-hook-owner-test-0.scope loaded active running Hook';; esac\nexit 0\n")
	external := worktreeWithRecordedScope(t, "")
	external.worktreePath = owner.worktreePath
	external.externalWorktree = true
	external.SetHookScopeSessionID("external")
	// Registered after the fixtures: even a failing assertion joins before
	// TempDir cleanup can race the watcher's finished-marker write.
	t.Cleanup(func() {
		owner.hooksCancel()
		if owner.HooksDone() != nil {
			waitForClosed(t, owner.HooksDone(), 5*time.Second, "owner watcher did not stop")
		}
	})
	AdoptRunningHooks([]*GitWorktree{external, owner})
	if external.HooksDone() != nil {
		t.Error("external session adopted managed hook progress")
	}
	if external.HookScopeUnitPrefix() != "" {
		t.Error("external session acquired managed scope identity")
	}
	requireOpen(t, owner.HooksDone(), "owner did not adopt its survivor")
	if err := external.CancelAndJoinHooks(); err != nil {
		t.Error(err)
	}
	raw, _ := os.ReadFile(managerLog)
	if strings.Contains(string(raw), " stop ") {
		t.Fatalf("external cancellation stopped managed scope: %s", raw)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitForClosed(t, owner.HooksDone(), 5*time.Second, "owner watcher did not finish after scope completion")
}

func TestHookProgressWrongOwnerCannotResume(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "")
	g.SetHookScopeSessionID("other")
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"echo ran > " + shellQuoteForShim(marker)}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	AdoptRunningHooks([]*GitWorktree{g})
	if g.HooksDone() != nil {
		waitForClosed(t, g.HooksDone(), 5*time.Second, "wrong owner's runner never finished")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("another session's repository command ran")
	}
	if g.HookScopeUnitPrefix() != "" {
		t.Fatal("wrong owner acquired the journal prefix")
	}
}

func TestHookProgressSafeTeardownReclaimsFinishedJournal(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(map[bool]string{false: "kill", true: "archive"}[archive], func(t *testing.T) {
			claimDaemonProcess(t)
			installScopeShim(t)
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			g := worktreeWithRecordedScope(t, "af-hook-owner")
			g.SetHookScopeSessionID("owner")
			g.repoPath = createGitRepo(t)
			p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p.receipt(0), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p.receipt(0), "exit"), []byte("0\n"), 0600); err != nil {
				t.Fatal(err)
			}
			p.finish()
			if archive {
				err = g.CancelAndJoinHooks()
			} else {
				_, err = g.Cleanup()
			}
			if err != nil {
				t.Fatal(err)
			}
			path, _ := hookProgressPath(g.worktreePath)
			for _, file := range []string{path, p.Directory} {
				if _, err := os.Stat(file); !os.IsNotExist(err) {
					t.Errorf("finished journal leaked after safe teardown: %s", file)
				}
			}
		})
	}
}
