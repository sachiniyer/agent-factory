//go:build linux

package git

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/hooklog"
)

func TestHookProgressCreateHoldBridgesOwnerCommit(t *testing.T) {
	for _, outcome := range []string{"commit", "abort"} {
		t.Run(outcome, func(t *testing.T) {
			claimDaemonProcess(t)
			installScopeShim(t)
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			// Owner scan and the liveness oracle are both injected: no outcome
			// below may wait on a real instances read or a real probe answering
			// inside a bound on a loaded runner (#7911).
			owners := stubHookProgressOwnerScan(t)
			oracle := stubHookProgressPruneOracle(t)
			tree := t.TempDir()
			g := &GitWorktree{worktreePath: tree}
			g.BeginHookCreatePersistence()
			p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner", leaseProgress: true}, []string{"true"}, "af-hook-owner", "test")
			if err != nil {
				t.Fatal(err)
			}
			g.retainHookProgressForCreate(p)
			originalOpen, originalWrite := openHookLog, hookProgressWriteFile
			originalProbe := runningHookPrefixesForResume
			openHookLog = func(hooklog.Kind) (*os.File, error) { return nil, errors.New("hook log unavailable") }
			runningHookPrefixesForResume = func(...string) ([]string, error) { return nil, errors.New("manager unavailable") }
			var failed atomic.Bool
			hookProgressWriteFile = func(path string, data []byte, mode os.FileMode) error {
				if filepath.Base(path) == "launch-failed" && !failed.Swap(true) {
					return errors.New("marker storage unavailable")
				}
				return originalWrite(path, data, mode)
			}
			t.Cleanup(func() {
				openHookLog, hookProgressWriteFile = originalOpen, originalWrite
				runningHookPrefixesForResume = originalProbe
				g.SettleHookCreatePersistence()
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
			waitForHookTestCondition(t, 5*time.Second, failed.Load, "create bailout receipt failure was not reached")
			waitForHookTestCondition(t, 5*time.Second, func() bool {
				p.leaseMu.Lock()
				defer p.leaseMu.Unlock()
				return p.leaseHolds == 2
			}, "runner and pending create did not retain both lease holds")
			requireOpen(t, done, "resumable create bailout reported completion")
			lease, err := os.OpenFile(filepath.Join(p.Directory, "runner.lock"), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
				_ = lease.Close()
				t.Fatalf("create did not retain its pre-commit lease: %v", err)
			}
			_ = lease.Close()
			path, err := hookProgressPath(tree)
			if err != nil {
				t.Fatal(err)
			}
			pruneHookProgress(filepath.Dir(path), time.Now().Add(48*time.Hour))
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("pending create journal was pruned: %v", err)
			}
			ownerRows := `[]`
			if outcome == "commit" {
				ownerRows = `[{"id":"owner"}]`
			}
			owners.set(map[string]json.RawMessage{"create-owner": json.RawMessage(ownerRows)})
			g.SettleHookCreatePersistence()
			cancel()
			waitForClosed(t, done, 5*time.Second, "cancelled create bailout did not close")
			later := time.Now().Add(48 * time.Hour)
			// An UNKNOWN answer — oracle or owner scan — keeps the journal for
			// either outcome (#7911).
			oracle.set(pruneOracleUnknown)
			pruneHookProgress(filepath.Dir(path), later)
			owners.setUnknown()
			pruneHookProgress(filepath.Dir(path), later)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("unknown liveness pruned the journal: %v", err)
			}
			owners.set(map[string]json.RawMessage{"create-owner": json.RawMessage(ownerRows)})
			oracle.set(pruneOracleDead)
			pruneHookProgress(filepath.Dir(path), later)
			_, err = os.Stat(path)
			if outcome == "commit" && err != nil {
				t.Fatalf("committed owner did not protect its journal: %v", err)
			}
			if outcome == "abort" && !os.IsNotExist(err) {
				t.Fatalf("aborted create journal was not reclaimable: %v", err)
			}
		})
	}
}

func TestAbandonHookProgressRetriesTerminalMarker(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// The archived owner row and the liveness oracle are both injected: the
	// reclamation assertion below must never wait on a real instances read or
	// a real systemctl probe answering inside a bound (#7911).
	owners := stubHookProgressOwnerScan(t)
	oracle := stubHookProgressPruneOracle(t)
	owners.set(map[string]json.RawMessage{"archived-owner": json.RawMessage(`[{"id":"owner","liveness":5}]`)})
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	originalMark := hookProgressMarkFinished
	var failed atomic.Bool
	hookProgressMarkFinished = func(path string, data []byte, mode os.FileMode) error {
		if !failed.Swap(true) {
			return errors.New("marker storage unavailable")
		}
		return originalMark(path, data, mode)
	}
	t.Cleanup(func() { hookProgressMarkFinished = originalMark })
	g.AbandonHookProgress()
	if g.hooksRetirementDone == nil {
		t.Fatal("failed abandonment did not schedule a retry")
	}
	waitForClosed(t, g.hooksRetirementDone, 5*time.Second, "abandonment retry did not finish")
	if !p.finished() {
		t.Fatal("abandonment retry did not write the terminal marker")
	}
	path, err := hookProgressPath(g.worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(48 * time.Hour)
	// An UNKNOWN answer — oracle or owner scan — must keep the journal (#7911).
	oracle.set(pruneOracleUnknown)
	pruneHookProgress(filepath.Dir(path), later)
	owners.setUnknown()
	pruneHookProgress(filepath.Dir(path), later)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unknown liveness pruned the journal: %v", err)
	}
	owners.set(map[string]json.RawMessage{"archived-owner": json.RawMessage(`[{"id":"owner","liveness":5}]`)})
	oracle.set(pruneOracleDead)
	pruneHookProgress(filepath.Dir(path), later)
	for _, stale := range []string{path, p.Directory} {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("abandoned archived journal was not reclaimable: %s: %v", stale, err)
		}
	}
}

func TestAbandonHookProgressWithoutFailureRemainsSynchronous(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	g.AbandonHookProgress()
	if !p.finished() || g.hooksRetirementDone != nil {
		t.Fatal("ordinary archived abandonment changed its synchronous behavior")
	}
}
