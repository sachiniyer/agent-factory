//go:build linux

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookProgressInconclusiveLivenessKeepsSuffixPending(t *testing.T) {
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
	if err := os.Mkdir(p.receipt(0), 0700); err != nil {
		t.Fatal(err)
	}
	originalProbe := runningHookPrefixesForResume
	probed := make(chan struct{})
	var probedOnce sync.Once
	runningHookPrefixesForResume = func(...string) ([]string, error) {
		probedOnce.Do(func() { close(probed) })
		return nil, errors.New("manager unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	t.Cleanup(func() {
		cancel()
		waitForClosed(t, done, 5*time.Second, "inconclusive claimant waiter did not stop")
		runningHookPrefixesForResume = originalProbe
	})
	select {
	case <-probed:
	case <-time.After(5 * time.Second):
		t.Fatal("claimant liveness was not probed")
	}
	time.Sleep(3 * hookAdoptionPollInterval)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("later entry ran while claimant liveness was inconclusive: %v", err)
	}
	requireOpen(t, done, "inconclusive claimant liveness reported completion")
	if p.finished() {
		t.Fatal("inconclusive claimant liveness marked the journal finished")
	}
}

func TestHookProgressLauncherCollisionDoesNotStopWinningScope(t *testing.T) {
	claimDaemonProcess(t)
	logPath := installScopeShim(t)
	fastHookAdoptionPoll(t)
	shimDir := filepath.Dir(logPath)
	launcherEntered := filepath.Join(t.TempDir(), "launcher-entered")
	releaseLauncher := filepath.Join(t.TempDir(), "release-launcher")
	launcher := `#!/bin/sh
set -eu
if [ "${1:-}" = "--help" ]; then printf '%s\n' '    --expand-environment=BOOL'; exit 0; fi
if [ ! -f ` + shellQuoteForShim(launcherEntered) + ` ]; then
    : > ` + shellQuoteForShim(launcherEntered) + `
    while [ ! -f ` + shellQuoteForShim(releaseLauncher) + ` ]; do sleep 1; done
    exit 1
fi
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
	originalStop, originalProbe := stopHookScopeUnits, runningHookPrefixesForResume
	claimLive := atomic.Bool{}
	claimLive.Store(true)
	probeReached := make(chan struct{})
	var probeOnce sync.Once
	runningHookPrefixesForResume = func(...string) ([]string, error) {
		probeOnce.Do(func() { close(probeReached) })
		if claimLive.Load() {
			return []string{p.Prefix}, nil
		}
		return nil, nil
	}
	stopCalled := atomic.Bool{}
	stopHookScopeUnits = func(...string) error {
		stopCalled.Store(true)
		claimLive.Store(false)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	t.Cleanup(func() {
		cancel()
		waitForClosed(t, done, 5*time.Second, "launcher-collision runner did not stop")
		stopHookScopeUnits, runningHookPrefixesForResume = originalStop, originalProbe
	})
	waitForPath(t, launcherEntered, 5*time.Second)
	if err := os.Mkdir(p.receipt(0), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseLauncher, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-probeReached:
	case <-time.After(5 * time.Second):
		t.Fatal("failed launcher did not enter competing-claim recovery")
	}
	if stopCalled.Load() {
		t.Fatal("losing launcher stopped the winning scope")
	}
	requireOpen(t, done, "losing launcher reported the winning claim complete")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("suffix ran while the winning scope was live: %v", err)
	}
	if err := config.AtomicWriteFile(filepath.Join(p.receipt(0), "exit"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	claimLive.Store(false)
	waitForClosed(t, done, 5*time.Second, "runner did not advance after the winner became terminal")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("suffix did not run after the winner became terminal: %v", err)
	}
}
