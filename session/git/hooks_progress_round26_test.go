//go:build linux

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/hooklog"
)

func TestHookProgressUnclaimedLauncherFailureAdvancesSuffix(t *testing.T) {
	claimDaemonProcess(t)
	logPath := installScopeShim(t)
	fastHookAdoptionPoll(t)
	shimDir := filepath.Dir(logPath)
	launcher := `#!/bin/sh
set -eu
if [ "${1:-}" = "--help" ]; then printf '%s\n' '    --expand-environment=BOOL'; exit 0; fi
printf '%s\n' "$*" >> ` + shellQuoteForShim(logPath) + `
case "$*" in *-0.scope*) sleep 1; exit 1 ;; esac
while [ "$#" -gt 0 ]; do
    case "$1" in
        --user|--scope|--quiet|--collect|--expand-environment=no|--unit=*|--property=*) shift ;;
        --) shift; break ;;
        *) exit 64 ;;
    esac
done
exec "$@"
`
	if err := os.WriteFile(filepath.Join(shimDir, "systemd-run"), []byte(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	marker := filepath.Join(tree, "second")
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{
		"true", "touch " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	originalProbe := runningHookPrefixesForResume
	runningHookPrefixesForResume = func(...string) ([]string, error) { return nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	t.Cleanup(func() {
		cancel()
		waitForClosed(t, done, 5*time.Second, "unclaimed-launcher runner did not stop")
		runningHookPrefixesForResume = originalProbe
	})
	waitForClosed(t, done, 3500*time.Millisecond, "unclaimed launcher failure retried instead of becoming terminal")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("suffix did not run after terminal launcher failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.receipt(0), "launch-failed")); err != nil {
		t.Fatalf("launcher failure was not recorded: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(logData), "-0.scope"); count != 1 {
		t.Fatalf("failed launcher attempts = %d, want exactly 1", count)
	}
}

func TestHookProgressRetiredNameIsSyncedBeforeReceiptRemoval(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	path, err := hookProgressPath(p.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	originalSync := hookProgressSyncDirectory
	injected := errors.New("retired journal directory sync failed")
	hookProgressSyncDirectory = func(string) error { return injected }
	t.Cleanup(func() { hookProgressSyncDirectory = originalSync })
	err = removeHookProgress(path, p)
	if !errors.Is(err, injected) {
		t.Fatalf("retirement error = %v, want directory sync failure", err)
	}
	if _, err := os.Stat(p.Directory); err != nil {
		t.Fatalf("receipts were removed before retired name was durable: %v", err)
	}
	retired := filepath.Join(filepath.Dir(path), "retired-"+filepath.Base(p.Directory)+".json")
	if _, err := os.Stat(retired); err != nil {
		t.Fatalf("retired journal was not published before sync: %v", err)
	}
}

func TestHookProgressFailedClaimSyncKeepsSuffixPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	marker := filepath.Join(tree, "second")
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{
		"true", "touch " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	originalOpen, originalSync := openHookLog, hookProgressSyncDirectory
	openHookLog = func(hooklog.Kind) (*os.File, error) { return nil, errors.New("hook log unavailable") }
	syncAttempted := make(chan struct{})
	var syncOnce sync.Once
	hookProgressSyncDirectory = func(string) error {
		syncOnce.Do(func() { close(syncAttempted) })
		return errors.New("receipt parent sync failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	t.Cleanup(func() {
		cancel()
		waitForClosed(t, done, 5*time.Second, "failed-claim sync runner did not stop")
		openHookLog, hookProgressSyncDirectory = originalOpen, originalSync
	})
	select {
	case <-syncAttempted:
	case <-time.After(3 * time.Second):
		t.Fatal("failed-claim durability sync was not attempted")
	}
	time.Sleep(3 * hookAdoptionPollInterval)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("suffix ran before the failed claim was durable: %v", err)
	}
	requireOpen(t, done, "failed-claim sync failure reported completion")
	if p.finished() {
		t.Fatal("failed-claim sync failure marked the journal finished")
	}
}

func TestHookProgressScopeProbeDoesNotHoldPublicationLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	p.finish()
	path, err := hookProgressPath(p.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, stale := range []string{path, p.Directory, filepath.Join(p.Directory, "finished")} {
		if err := os.Chtimes(stale, old, old); err != nil {
			t.Fatal(err)
		}
	}
	entered := filepath.Join(t.TempDir(), "probe-entered")
	release := filepath.Join(t.TempDir(), "probe-release")
	installSurvivorSystemctl(t, `: > `+shellQuoteForShim(entered)+`
while [ ! -f `+shellQuoteForShim(release)+` ]; do sleep 1; done
exit 0
`)
	pruneDone := make(chan struct{})
	go func() {
		pruneHookProgress(filepath.Dir(path), time.Now())
		close(pruneDone)
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0600)
		waitForClosed(t, pruneDone, 5*time.Second, "blocked retention probe did not drain")
	})
	waitForPath(t, entered, 5*time.Second)
	acquired, err := config.TryWithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("slow scope probe retained the publication lock")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitForClosed(t, pruneDone, 5*time.Second, "retention did not finish after scope probe recovered")
}
