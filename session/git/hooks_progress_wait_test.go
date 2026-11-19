//go:build linux

package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
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
	var retirementErr error
	go func() { retirementErr = g.retireHookProgress(); close(done) }()
	// Release and join even on the expected pre-fix failure, before TempDir cleanup.
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
			<-lockDone
		}
		<-done
		if g.hooksRetirementDone != nil {
			<-g.hooksRetirementDone
		}
	})
	select {
	case <-done:
		if retirementErr != nil {
			t.Fatal(retirementErr)
		}
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
	// Archive changes this field after teardown; the retry must retain the old journal path.
	g.worktreePath = filepath.Join(t.TempDir(), "archived")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if g.hooksRetirementDone != nil {
		waitForClosed(t, g.hooksRetirementDone, 3*time.Second, "retirement retry did not finish")
	}
	for _, file := range []string{path, p.Directory} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("later retirement left %s: %v", file, err)
		}
	}
}

// The outage episode must be logged once no matter how many DIFFERENT failure
// messages it contains. The sighted flake (#4217, run 44530803803): a scripted
// systemctl outage answered by the manager oracle overlapped one real argv
// refusal from the launcher oracle, so the probe's messages went outage →
// unreadable argv → outage and the message-keyed dedup warned three times.
// This test scripts exactly that sequence — deterministic on every platform
// because the probe is stubbed rather than shimmed through the environment.
func TestHookProgressProbeOutageLogsOnceAndRecovers(t *testing.T) {
	fastHookAdoptionPoll(t)
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath = linkedHookWorktree(t)
	g.branchName = "hook-resume"
	if _, err := newHookProgress(hookRun{repoPath: g.repoPath, worktreePath: g.worktreePath, scopeSessionID: "owner"}, nil, "af-hook-owner", "test"); err != nil {
		t.Fatal(err)
	}
	outage := errors.New("systemctl --user list-units: exit status 1: manager unavailable")
	unreadable := fmt.Errorf("cannot rule out a hook launcher under af-hook-owner: %w",
		&proctree.ArgvUnreadableError{PID: 4242, Err: proctree.ErrNoArgv, OwnedByUs: true})
	var probes atomic.Int32
	originalProbe := runningHookPrefixesForResume
	runningHookPrefixesForResume = func(...string) ([]string, error) {
		switch n := probes.Add(1); {
		case n <= 4:
			return nil, outage
		case n <= 10:
			return nil, unreadable
		case n <= 20:
			return nil, outage
		default:
			return nil, nil
		}
	}
	t.Cleanup(func() { runningHookPrefixesForResume = originalProbe })
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
	if strings.Contains(warnings.String(), "cannot rule out a hook launcher") {
		t.Errorf("the mid-outage argv failure became a second warning: %s", warnings.String())
	}
	if got := strings.Count(infos.String(), "hook scope probe still failing"); got != 2 {
		t.Errorf("changed mid-episode reasons demoted to info = %d, want 2: %s", got, infos.String())
	}
	if !strings.Contains(infos.String(), "cannot rule out a hook launcher") {
		t.Errorf("the changed reason never reached the log at all: %s", infos.String())
	}
	if got := strings.Count(infos.String(), "hook scope probe recovered"); got != 1 {
		t.Errorf("recovery messages = %d, want 1: %s", got, infos.String())
	}
	if n := probes.Load(); n < 21 {
		t.Errorf("watcher stopped probing at %d, want the full outage observed (>= 21)", n)
	}
}
