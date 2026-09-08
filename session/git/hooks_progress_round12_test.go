//go:build linux

package git

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/hooklog"
)

func TestHookProgressOpenFailureRemainsResumable(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	marker := filepath.Join(tree, "second")
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{"true", "echo second >> " + shellQuoteForShim(marker)}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	originalOpen, originalWrite := openHookLog, hookProgressWriteFile
	var opened atomic.Bool
	openHookLog = func(kind hooklog.Kind) (*os.File, error) {
		if !opened.Swap(true) {
			return nil, errors.New("hook log unavailable")
		}
		return originalOpen(kind)
	}
	var failed atomic.Bool
	hookProgressWriteFile = func(path string, data []byte, mode os.FileMode) error {
		if filepath.Base(path) == "launch-failed" && !failed.Swap(true) {
			return errors.New("marker storage unavailable")
		}
		return originalWrite(path, data, mode)
	}
	t.Cleanup(func() {
		openHookLog, hookProgressWriteFile = originalOpen, originalWrite
	})
	done := runPostWorktreeHooks(t.Context(), hookRun{worktreePath: tree, progress: p})
	waitForClosed(t, done, 5*time.Second, "failed launch runner did not stop")
	if p.finished() {
		t.Fatal("hook log failure marked an unfinished journal complete")
	}
	if p.claimed(0) {
		t.Fatal("partial failed-launch receipt made the entry permanently claimed")
	}
	resumed := runPostWorktreeHooks(t.Context(), hookRun{worktreePath: tree, progress: p})
	waitForClosed(t, resumed, 5*time.Second, "resumed suffix did not finish")
	if !p.finished() {
		t.Fatal("resumed journal did not finish")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("remaining command did not run: %v", err)
	}
}

func TestHookProgressScopeStopFailureKeepsDoneOpenUntilScopeGone(t *testing.T) {
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
	marker := filepath.Join(tree, "second")
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{"true", "echo second >> " + shellQuoteForShim(marker)}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	originalStop, originalProbe := stopHookScopeUnits, runningHookPrefixesForResume
	var gone atomic.Bool
	var stops atomic.Int32
	stopHookScopeUnits = func(...string) error { stops.Add(1); return errors.New("manager unavailable") }
	var probes atomic.Int32
	runningHookPrefixesForResume = func(...string) ([]string, error) {
		probes.Add(1)
		if gone.Load() {
			return nil, nil
		}
		return []string{"af-hook-owner"}, nil
	}
	previousTimeout, previousPoll := hookStopTimeout, hookAdoptionPollInterval
	hookStopTimeout, hookAdoptionPollInterval = time.Second, 10*time.Millisecond
	t.Cleanup(func() {
		stopHookScopeUnits, runningHookPrefixesForResume = originalStop, originalProbe
		hookStopTimeout, hookAdoptionPollInterval = previousTimeout, previousPoll
	})
	done := runPostWorktreeHooks(t.Context(), hookRun{worktreePath: tree, progress: p})
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatalf("HooksDone closed while the scope was still uncertain (stops=%d probes=%d)", stops.Load(), probes.Load())
	default:
	}
	gone.Store(true)
	waitForClosed(t, done, 5*time.Second, "scope recovery did not resume suffix")
	if !p.finished() {
		t.Fatal("scope recovery did not finish the journal")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("remaining command did not run: %v", err)
	}
}
