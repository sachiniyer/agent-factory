package daemon

import (
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// PR discovery is daemon-side (#3232). Before this loop the only producer of
// the pr_info projection was the TUI's background fetch for its selected
// session, so a session used only from the web or the CLI never had its PR
// discovered at all — the daemon persisted whatever a TUI happened to hand it,
// and no TUI meant nothing. The daemon now discovers PR info itself, through
// the same `gh` lookup and the same Manager.SetPRInfo write path (op-lock,
// archived-row refusal, persist-then-publish), so every surface reads a
// projection the daemon keeps current on its own.
//
// The TUI still fetches its SELECTED session faster (60s); both producers
// funnel through Manager.SetPRInfo, whose instance write bumps the shared
// freshness clock — which is exactly what keeps this sweep from re-fetching a
// session a TUI just refreshed.

const (
	// prInfoSweepInterval is how often the daemon scans sessions for stale PR
	// info. Scanning is cheap; fetching is not — the staleness window below is
	// what bounds the network work.
	prInfoSweepInterval = time.Minute
	// prInfoSweepStaleAfter is how old a session's PR info may grow before the
	// sweep refreshes it. Deliberately lazier than the TUI's 60s
	// selected-session refresh: the sweep covers EVERY local session, each
	// refresh is a `gh` network call against the shared GitHub API budget, so
	// the window caps background usage at sessions × 6 calls/hour while a
	// web/CLI-only workflow still converges within minutes.
	prInfoSweepStaleAfter = 10 * time.Minute
)

// prInfoFetchFn is the discovery seam, a package var for the same reason as
// the TUI's fetcher seam: the real function shells out to `gh`, which tests
// must not do.
var prInfoFetchFn = git.FetchPRInfo

// startPRInfoRefreshLoop runs the PR-info sweep until stopCh closes, mirroring
// startInstancePollLoop's shape: the body runs once immediately (a restored
// daemon has only zero-valued freshness clocks, so the first sweep is what
// populates badges after a restart), then once per tick.
func startPRInfoRefreshLoop(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(prInfoSweepInterval)
		defer ticker.Stop()
		for {
			manager.RefreshStalePRInfo()
			select {
			case <-stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
}

// RefreshStalePRInfo refreshes PR info for every eligible session whose entry
// has gone stale, one fetch at a time so the sweep never bursts `gh`
// subprocesses.
func (m *Manager) RefreshStalePRInfo() {
	// The same lifecycle admission every mutation consults (#3231): during
	// upgrade probation or quiescing the metadata snapshot must stay coherent,
	// so the sweep writes nothing — and fetches nothing, since a result it
	// cannot record is wasted network.
	if m.lifecycle != nil && m.lifecycle.mutationAdmissionError() != nil {
		return
	}
	if !m.Ready() {
		return
	}
	type entry struct {
		repoID   string
		instance *session.Instance
	}
	m.mu.Lock()
	entries := make([]entry, 0, len(m.instances))
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		entries = append(entries, entry{repoID: repoID, instance: inst})
	}
	m.mu.Unlock()
	for _, e := range entries {
		m.refreshInstancePRInfo(e.repoID, e.instance)
	}
}

// refreshInstancePRInfo fetches and records one session's PR info if it is
// eligible and stale. Eligibility mirrors the TUI's fetch gate: PR info comes
// from `gh` against a local branch, so only a started local-worktree session
// with a branch qualifies, and rows that are archived, tearing down, or killed
// have no use for a badge (their write would be refused anyway).
func (m *Manager) refreshInstancePRInfo(repoID string, inst *session.Instance) {
	if inst.Capabilities().Workspace != session.WorkspaceLocalWorktree {
		return
	}
	if inst.IsArchived() || inst.IsTearingDown() || inst.UserKilled() {
		return
	}
	if inst.PRInfoAge() < prInfoSweepStaleAfter {
		return
	}
	repoPath, branch := inst.FetchPRInfoSnapshot()
	if repoPath == "" || branch == "" {
		return
	}
	// Bump the freshness clock at kickoff, exactly as the TUI does: a failed or
	// empty fetch then waits out a full staleness window instead of retrying on
	// every sweep while the network is down.
	inst.MarkPRInfoFetched()
	info, err := prInfoFetchFn(repoPath, branch)
	if err != nil {
		log.WarningLog.Printf("PR info sweep: fetch for %q failed: %v", inst.Title, err)
		return
	}
	if info != nil {
		// Bind the result to the ref it was looked up for, like the TUI's
		// fetch: a PR state without provenance must never authorize anything.
		bound := *info
		bound.Branch = branch
		info = &bound
	}
	// An unchanged result writes nothing: persisting and publishing it anyway
	// would emit a session.updated per session per sweep with zero information.
	if prInfoEqual(inst.GetPRInfo(), info) {
		return
	}
	// The branch can move while `gh` runs; a result for the old ref must not be
	// recorded against the new one (the TUI drops this case too).
	if _, current := inst.FetchPRInfoSnapshot(); current != branch {
		return
	}
	var data session.PRInfoData
	if info != nil {
		data = session.PRInfoData{
			Number: info.Number,
			Title:  info.Title,
			URL:    info.URL,
			State:  info.State,
			Branch: info.Branch,
		}
	}
	// SetPRInfo is the one write path (#2437): identity re-resolution by stable
	// id, op-lock against archive/kill teardown, archived-row refusal, persist,
	// then publish — the sweep gets every guard the client write path has.
	if err := m.SetPRInfo(SetPRInfoRequest{RepoID: repoID, Title: inst.Title, ID: inst.ID, PRInfo: data}); err != nil {
		log.WarningLog.Printf("PR info sweep: record for %q failed: %v", inst.Title, err)
	}
}

func prInfoEqual(a, b *git.PRInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
