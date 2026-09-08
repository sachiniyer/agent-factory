//go:build linux

package git

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

func TestHookProgressRetireDoesNotWaitForPublisher(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	p.finish()
	path, _ := hookProgressPath(g.worktreePath)
	locked, release, lockDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		lockDone <- config.WithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-lockDone:
		t.Fatalf("publisher lock failed: %v", err)
	}
	done := make(chan struct{})
	go func() { g.retireHookProgress(); close(done) }()
	// Release and join even on the expected pre-fix failure, before TempDir cleanup.
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
			<-lockDone
		}
		<-done
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retirement waited for the publisher lock")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("contended retirement removed journal: %v", err)
	}
	if !p.completed() {
		t.Fatal("contended retirement changed completed receipts")
	}
	// Allow a later pass to retry without waiting for this test's cleanup.
	close(release)
	lockErr := <-lockDone
	released = true
	if lockErr != nil {
		t.Fatal(lockErr)
	}
	g.retireHookProgress()
	for _, file := range []string{path, p.Directory} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("later retirement left %s: %v", file, err)
		}
	}
}

func TestHookProgressProbeOutageLogsOnceAndRecovers(t *testing.T) {
	fastHookAdoptionPoll(t)
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath = linkedHookWorktree(t)
	g.branchName = "hook-resume"
	if _, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, nil, "af-hook-owner", "test"); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(t.TempDir(), "probes")
	installSurvivorSystemctl(t, fmt.Sprintf(`n=0
if [ -f %q ]; then read -r n < %q; fi
n=$((n + 1))
printf '%%s\n' "$n" > %q
if [ "$n" -le 20 ]; then echo 'manager unavailable' >&2; exit 1; fi
exit 0
`, counter, counter, counter))
	var warnings, infos bytes.Buffer
	warningWriter, infoWriter := log.WarningLog.Writer(), log.InfoLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	log.InfoLog.SetOutput(&infos)
	t.Cleanup(func() {
		log.WarningLog.SetOutput(warningWriter)
		log.InfoLog.SetOutput(infoWriter)
	})
	t.Cleanup(func() {
		g.hooksCancel()
		if g.HooksDone() != nil {
			waitForClosed(t, g.HooksDone(), 5*time.Second, "watcher did not stop")
		}
	})
	if !g.adoptHookProgress() {
		t.Fatal("journal was not adopted")
	}
	waitForClosed(t, g.HooksDone(), 10*time.Second, "watcher did not recover")
	if got := strings.Count(warnings.String(), "waiting to resume post-worktree hooks"); got != 1 {
		t.Errorf("outage warnings = %d, want 1: %s", got, warnings.String())
	}
	if got := strings.Count(infos.String(), "hook scope probe recovered"); got != 1 {
		t.Errorf("recovery messages = %d, want 1: %s", got, infos.String())
	}
	raw, err := os.ReadFile(counter)
	if err != nil || strings.TrimSpace(string(raw)) != "21" {
		t.Errorf("probe count = %q, err %v, want 21", raw, err)
	}
}
