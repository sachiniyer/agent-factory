package session

import (
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// i.Account is rewritten at runtime by account swaps and handoffs under
// i.mu.Lock (selectAccountLocked, RestoreAccountSelectionUnderResumeFence,
// SwapAgentProgram). Every other reader in this package takes i.mu.RLock for it
// — AccountSelection, recordNamesAccount, refreshSessionIdentity — and
// handoff.go's AgentProgram documents the contract: "reads that can run
// concurrently with a swap must go through here. The write side holds i.mu; an
// unguarded read of the bare field races it." claudeStoreFor, resolveSkillTarget
// and prepareCreateLaunch missed the lock. These tests are the -race regression:
// a locked writer flipping i.Account must not race the reader path. CI runs
// `go test -race ./...`, so a reverted bare read redden the gate the way the
// pre-fix code did.
//
// The writer runs for the WHOLE duration of the reader (not a fixed count): a
// slow reader's own RLock sections (AccountAgent, guidanceCaps, GetWorktreePath)
// establish a happens-before edge from the writer's last release to a bare read
// that follows them, which would let a writer that already finished hide the
// race. A writer that keeps rewriting for as long as the reader reads leaves a
// write in every bare-read window, so the detector sees the unprotected access.

// raceAccountWriter is the write half of every race below: it mirrors the
// lock-protected writers (selectAccountLocked and friends) by rewriting
// i.Account under i.mu.Lock, toggling between name and empty, until stop is
// closed by the reader.
func raceAccountWriter(wg *sync.WaitGroup, i *Instance, stop <-chan struct{}, name string) {
	defer wg.Done()
	for {
		select {
		case <-stop:
			return
		default:
		}
		i.mu.Lock()
		if i.Account == "" {
			i.Account = name
		} else {
			i.Account = ""
		}
		i.mu.Unlock()
	}
}

// accountRaceRunner starts the writer against reader and waits for both. reader
// closes stop when its loop ends, so the writer rewrites i.Account for the
// reader's whole run.
func accountRaceRunner(i *Instance, iterations int, reader func()) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go raceAccountWriter(&wg, i, stop, "work")
	go func() {
		defer wg.Done()
		defer close(stop)
		for n := 0; n < iterations; n++ {
			reader()
		}
	}()
	wg.Wait()
}

// claudeStoreRaceInstance is a Claude session pinned to a registered "work"
// account, plus a launch directory claudeStoreFor resolves against. The
// instance needs no git worktree: judgeRecordedClaude takes the launch dir as an
// argument, and the empty-account verdict path does not consult one.
func claudeStoreRaceInstance(t *testing.T) (*Instance, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	registerAccount(t, tmux.ProgramClaude, "work")
	inst := &Instance{Title: "claude-store-race", Program: tmux.ProgramClaude, Account: "work"}
	return inst, t.TempDir()
}

// TestClaudeStoreForReadsAccountUnderLock reproduces the bare read in
// claudeStoreFor (claude_transcript.go): the recorded-id resolution reads
// i.Account with no i.mu.RLock while a swap/handoff writer rewrites it under
// i.mu.Lock. Under -race the pre-fix code aborts with WARNING: DATA RACE.
func TestClaudeStoreForReadsAccountUnderLock(t *testing.T) {
	inst, workDir := claudeStoreRaceInstance(t)
	accountRaceRunner(inst, 200, func() {
		_, _, _ = inst.claudeStoreFor(tmux.ProgramClaude, workDir)
	})
}

// TestJudgeRecordedClaudeReadsAccountUnderLock is the same race through the
// production restore/respawn path: recordedClaudeDecline -> judgeRecordedClaude
// -> claudeStoreFor reads i.Account. This is the path the poll and per-request
// RPC goroutines run on a live *Instance.
func TestJudgeRecordedClaudeReadsAccountUnderLock(t *testing.T) {
	inst, workDir := claudeStoreRaceInstance(t)
	const recordedID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba5"
	accountRaceRunner(inst, 200, func() {
		_, _ = inst.judgeRecordedClaude(tmux.ProgramClaude, workDir, recordedID)
	})
}

// TestRejudgeRecordedClaudeReadsAccountUnderLock is the race the detached
// re-judge goroutine widens: rejudgeRecordedClaude spawns a goroutine that runs
// judgeRecordedClaude (and so claudeStoreFor's i.Account read) and is
// deliberately not waited for when its deadline lapses. Even when the parent
// waits for the verdict, the child's read of i.Account runs on its own
// goroutine and must be locked against the swap/handoff writers.
func TestRejudgeRecordedClaudeReadsAccountUnderLock(t *testing.T) {
	inst, workDir := claudeStoreRaceInstance(t)
	const recordedID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba5"
	deadline := fsprobe.FlightNow().Add(2 * time.Second) // far enough that the child lands first
	accountRaceRunner(inst, 200, func() {
		_ = inst.rejudgeRecordedClaude(tmux.ProgramClaude, workDir, recordedID, deadline)
	})
}

// TestResolveSkillTargetReadsAccountUnderLock is the -race regression for the
// bare i.Account read in resolveSkillTarget (agentskill.go), reached by every
// local launch's injectSystemPrompt path.
func TestResolveSkillTargetReadsAccountUnderLock(t *testing.T) {
	agentHome(t)
	grantGlobalAgentSkills(t)
	registerAccount(t, tmux.ProgramClaude, "work")
	inst := skillTestInstance(t, tmux.ProgramClaude, "work")
	accountRaceRunner(inst, 200, func() {
		_ = resolveSkillTarget(inst, tmux.ProgramClaude)
	})
}

// TestPrepareCreateLaunchReadsAccountUnderLock is the -race regression for the
// bare i.Account read in prepareCreateLaunch (backend_local_create.go): the
// create plan reads the pinned account to refuse agent drift and validate the
// command, and runs on the create RPC goroutine while a concurrent swap could
// rewrite i.Account. The create's reader is slow (it resolves the launch
// command and env before the read), so a writer that keeps rewriting for the
// reader's whole run is what leaves a write in the read's window.
func TestPrepareCreateLaunchReadsAccountUnderLock(t *testing.T) {
	f := newFreshIDFixture(t)
	f.inst.Account = "work"
	accountRaceRunner(f.inst, 100, func() {
		_, _ = (&LocalBackend{}).prepareCreateLaunch(f.inst)
	})
}
