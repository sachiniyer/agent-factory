//go:build linux

package git

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookProgressDeadStartedClaimResumesSuffix(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	marker := filepath.Join(tree, "second")
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{
		"true", "touch " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.receipt(0), 0700); err != nil {
		t.Fatal(err)
	}
	previousTimeout, previousPoll := hookStopTimeout, hookAdoptionPollInterval
	originalProbe := runningHookPrefixesForResume
	hookStopTimeout = 50 * time.Millisecond
	hookAdoptionPollInterval = 10 * time.Millisecond
	runningHookPrefixesForResume = func(...string) ([]string, error) { return nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	t.Cleanup(func() {
		cancel()
		waitForClosed(t, done, 5*time.Second, "dead-claim runner did not stop")
		hookStopTimeout, hookAdoptionPollInterval = previousTimeout, previousPoll
		runningHookPrefixesForResume = originalProbe
	})
	waitForClosed(t, done, 3*time.Second, "dead started receipt was never terminalized")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("suffix did not resume after the dead claim: %v", err)
	}
	if state, err := p.entryState(0); err != nil || state != hookEntryFinished {
		t.Fatalf("dead claim state = %v, %v; want terminal failure", state, err)
	}
}

func TestHookProgressRetirementReleasesLockBeforeReceiptDeletion(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.markFinished(); err != nil {
		t.Fatal(err)
	}
	path, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	originalRemove := hookProgressRemove
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	hookProgressRemove = func(path string, progress *hookProgress) error {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return originalRemove(path, progress)
	}
	done := make(chan error, 1)
	resultReceived := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if !resultReceived {
			<-done
		}
		hookProgressRemove = originalRemove
	})
	go func() {
		_, retireErr := retireHookProgressSnapshot(p, path)
		done <- retireErr
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("receipt deletion did not start")
	}
	acquired, err := config.TryWithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("receipt deletion retained the publication lock: acquired=%v err=%v", acquired, err)
	}
	releaseOnce.Do(func() { close(release) })
	retireErr := <-done
	resultReceived = true
	if retireErr != nil {
		t.Fatal(retireErr)
	}
}
