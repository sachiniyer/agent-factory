package session

import (
	"time"

	"github.com/sachiniyer/agent-factory/session/git"
)

// GetPRInfo returns the associated GitHub PR info, or nil if none.
func (i *Instance) GetPRInfo() *git.PRInfo {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.prInfo
}

// SetPRInfo sets the associated GitHub PR info.
func (i *Instance) SetPRInfo(info *git.PRInfo) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prInfo = info
	i.prInfoLastFetched = time.Now()
	i.prInfoGeneration++
}

// PRInfoGeneration counts every write to this instance's PR info or its
// freshness clock. A slow producer captures it at kickoff and compares before
// recording, so a result that raced a NEWER producer's write is discarded
// instead of overwriting it (#3287 review) — the badge equivalent of a CAS
// expectation, best-effort because the final write happens under the write
// path's own locks.
func (i *Instance) PRInfoGeneration() uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.prInfoGeneration
}

// PRInfoRollback captures the complete pre-write PR-info state — value,
// freshness clock, and generation — so a write whose durable half failed can
// be undone without trace. Opaque on purpose: the fields only mean anything
// restored together.
type PRInfoRollback struct {
	info       *git.PRInfo
	fetchedAt  time.Time
	generation uint64
}

// BeginPRInfoWrite applies info exactly as SetPRInfo does and returns the
// rollback point for the state it replaced.
func (i *Instance) BeginPRInfoWrite(info *git.PRInfo) PRInfoRollback {
	i.mu.Lock()
	defer i.mu.Unlock()
	rollback := PRInfoRollback{info: i.prInfo, fetchedAt: i.prInfoLastFetched, generation: i.prInfoGeneration}
	i.prInfo = info
	i.prInfoLastFetched = time.Now()
	i.prInfoGeneration++
	return rollback
}

// RollbackPRInfoWrite reinstates the state BeginPRInfoWrite replaced — the
// generation and freshness clock included, not just the value. A failed
// persist committed nothing, so it must leave no trace: a generation left
// advanced would spuriously fail a concurrent producer's CAS and make it
// discard a still-valid result, and a refreshed clock would keep the old value
// looking fresh for another staleness window (#3287 review).
func (i *Instance) RollbackPRInfoWrite(rollback PRInfoRollback) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prInfo = rollback.info
	i.prInfoLastFetched = rollback.fetchedAt
	i.prInfoGeneration = rollback.generation
}

// PRInfoAge returns how long ago PR info was last fetched. Returns a very
// large duration if PR info has never been fetched in this process.
func (i *Instance) PRInfoAge() time.Duration {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.prInfoLastFetched.IsZero() {
		return time.Duration(1<<62 - 1)
	}
	return time.Since(i.prInfoLastFetched)
}

// MarkPRInfoFetched bumps the fetch timestamp without touching the cached
// value. Used after a transient fetch error so we don't re-try on every
// subsequent selection change.
func (i *Instance) MarkPRInfoFetched() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prInfoLastFetched = time.Now()
	i.prInfoGeneration++
}

// SetPRInfoFetchedAtForTest backdates the freshness clock so staleness-window
// tests can age an entry without sleeping. Test-only; nothing is derived from
// the timestamp beyond PRInfoAge itself.
func (i *Instance) SetPRInfoFetchedAtForTest(at time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prInfoLastFetched = at
}

// FetchPRInfoSnapshot returns the data needed to fetch PR info for this
// instance off the main event loop. The returned repoPath is empty when the
// instance is not ready for fetching (not started, no worktree, or remote).
func (i *Instance) FetchPRInfoSnapshot() (repoPath, branch string) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.started || i.gitWorktree == nil {
		return "", ""
	}
	// The GitWorktree branch is the canonical ref cleanup owns. Instance.Branch
	// is a legacy display field and can be stale on restored rows; fetching PR
	// state for it can later suppress a warning about the canonical branch.
	return i.gitWorktree.GetRepoPath(), i.gitWorktree.GetBranchName()
}
