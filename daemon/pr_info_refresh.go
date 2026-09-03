package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// PR discovery is daemon-side (#3232/#3296). The daemon is the only producer of
// the pr_info projection: its background sweep covers every local session, and
// clients may poke RefreshPRInfo for a selected session without supplying or
// deriving any PR fields. Both paths use the same fenced fetch and guarded
// persist-then-publish write, so every surface reads one authoritative value.

const (
	// prInfoSweepInterval is how often the daemon scans sessions for stale PR
	// info. Scanning is cheap; fetching is not — the staleness window below is
	// what bounds the network work.
	prInfoSweepInterval = time.Minute
	// prInfoSweepStaleAfter is how old a session's PR info may grow before the
	// sweep refreshes it. Deliberately lazier than the selected-session window:
	// the sweep covers EVERY local session, each
	// refresh is a `gh` network call against the shared GitHub API budget, so
	// the window caps background usage at sessions × 6 calls/hour while a
	// web/CLI-only workflow still converges within minutes.
	prInfoSweepStaleAfter = 10 * time.Minute
	// prInfoPokeStaleAfter keeps a selected session fresher without allowing
	// several clients (or rapid selection events) to multiply gh calls. Leave
	// margin below the TUI's minute tick so scheduler jitter cannot suppress
	// every other refresh.
	prInfoPokeStaleAfter = 55 * time.Second
)

// prInfoFetchFn is the daemon discovery seam. The real function shells out to
// `gh`, which tests must not do. Context-taking so either a request disconnect
// or daemon shutdown can abandon an in-flight lookup immediately.
var prInfoFetchFn = git.FetchPRInfoContext

// startPRInfoRefreshLoop runs the PR-info sweep until stopCh closes, mirroring
// startInstancePollLoop's shape: the body runs once immediately (a restored
// daemon has only zero-valued freshness clocks, so the first sweep is what
// populates badges after a restart), then once per tick.
//
// stopCh is threaded INTO the sweep as a context, not just consulted between
// ticks: a serialized sweep over N sessions with a stalled network could
// otherwise hold `gh` subprocesses for N × the network timeout after shutdown
// began, while runDaemon blocks in wg.Wait() ahead of its final persistence —
// and the stop/upgrade path escalates to a kill long before that drains.
func startPRInfoRefreshLoop(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The watcher exits when stopCh closes — the only way the loop below
		// ends — so it never outlives this goroutine's purpose.
		go func() {
			<-stopCh
			cancel()
		}()
		ticker := time.NewTicker(prInfoSweepInterval)
		defer ticker.Stop()
		for {
			manager.refreshStalePRInfo(ctx)
			select {
			case <-stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
}

// refreshStalePRInfo refreshes PR info for every eligible session whose entry
// has gone stale, one fetch at a time so the sweep never bursts `gh`
// subprocesses. It returns as soon as ctx is cancelled — between entries here,
// and mid-fetch via the context handed to `gh`.
func (m *Manager) refreshStalePRInfo(ctx context.Context) {
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
	// `gh` missing is "discovery unavailable", not "no PR anywhere" — but
	// FetchPRInfo deliberately folds it into (nil, nil), which downstream reads
	// as an authoritative empty result. Left unguarded, a daemon whose service
	// PATH lost `gh` would CLEAR every persisted badge on its first sweep
	// (restored freshness clocks are zero, so everything looks stale). Skip the
	// sweep outright and keep the cached values (#3287 review).
	if _, err := exec.LookPath("gh"); err != nil {
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
		if ctx.Err() != nil {
			return
		}
		if err := m.refreshInstancePRInfo(ctx, e.repoID, e.instance, prInfoSweepStaleAfter); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				IsDaemonQuiescingErr(err) || IsDaemonUpgradeProbationErr(err) {
				return
			}
			m.warn().Printf("PR info sweep: refresh for %q failed: %v", e.instance.Title, err)
		}
	}
}

// RefreshPRInfo handles a client poke for one session. It deliberately accepts
// no PR payload: the daemon resolves the canonical row, owns the eligibility
// decision, runs discovery with the caller's cancellation context, and records
// through the same guarded write as the background sweep.
func (m *Manager) RefreshPRInfo(ctx context.Context, req RefreshPRInfoRequest) error {
	if m.lifecycle != nil {
		if err := m.lifecycle.mutationAdmissionError(); err != nil {
			return err
		}
	}
	inst, repoID, _, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return err
	}
	if inst == nil {
		return errors.New("failed to resolve session for PR-info refresh")
	}
	// Missing gh means discovery is unavailable, not an authoritative "no PR".
	// Preserve the last projection, but tell the requesting client: unlike the
	// sweep, it has a user-facing error path and no fallback producer (#3296).
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("PR info discovery requires gh in the daemon PATH: %w", err)
	}
	return m.refreshInstancePRInfo(ctx, repoID, inst, prInfoPokeStaleAfter)
}

// RefreshPRInfo is the net/rpc entrypoint. The HTTP route calls refreshPRInfo
// directly so a disconnected client cancels the gh subprocess.
func (s *controlServer) RefreshPRInfo(req RefreshPRInfoRequest, resp *RefreshPRInfoResponse) error {
	return s.refreshPRInfo(context.Background(), req, resp)
}

func (s *controlServer) refreshPRInfo(
	ctx context.Context,
	req RefreshPRInfoRequest,
	resp *RefreshPRInfoResponse,
) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	if err := s.manager.RefreshPRInfo(ctx, req); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

// refreshInstancePRInfo fetches and records one session's PR info if it is
// eligible and stale. PR info comes from `gh` against a local branch, so only a
// started local-worktree session
// with a branch qualifies, and rows that are archived, tearing down, or killed
// have no use for a badge (their write would be refused anyway).
func (m *Manager) refreshInstancePRInfo(
	ctx context.Context,
	repoID string,
	inst *session.Instance,
	staleAfter time.Duration,
) error {
	if inst.Capabilities().Workspace != session.WorkspaceLocalWorktree {
		return nil
	}
	if inst.IsArchived() || inst.IsTearingDown() || inst.UserKilled() {
		return nil
	}
	repoPath, branch := inst.FetchPRInfoSnapshot()
	if repoPath == "" || branch == "" {
		return nil
	}
	// Claim freshness and capture the write fence in one critical section. This
	// is server-side because several TUI/web clients may poke the same session;
	// only one may launch gh inside the window.
	fetchClaim, claimed := inst.BeginPRInfoFetch(staleAfter)
	if !claimed {
		return nil
	}
	info, err := prInfoFetchFn(ctx, repoPath, branch)
	if ctx.Err() != nil {
		inst.CancelPRInfoFetch(fetchClaim)
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("fetch PR info for %q: %w", inst.Title, err)
	}
	if info != nil {
		// Bind the result to the exact ref the daemon looked up: a PR state
		// without provenance must never authorize anything.
		bound := *info
		bound.Branch = branch
		info = &bound
	}
	// The branch can move while `gh` runs; a result for the old ref must not be
	// recorded against the new one.
	if _, current := inst.FetchPRInfoSnapshot(); current != branch {
		inst.CancelPRInfoFetch(fetchClaim)
		return nil
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
	// The guarded write is the one write path (#2437) plus the sweep's
	// conditions evaluated UNDER its locks (#3287 review): the generation CAS
	// (a newer producer's result outranks this fetch), lifecycle admission (a
	// quiesce that began while `gh` ran refuses the record), the
	// unchanged-result skip (decided against committed state only), and a
	// cancellable lock wait (shutdown never blocks behind a teardown holding
	// the op-lock). The refusals are expected outcomes, not failures.
	err = m.setPRInfoGuarded(ctx,
		SetPRInfoRequest{RepoID: repoID, Title: inst.Title, ID: inst.ID, PRInfo: data},
		&prInfoWriteGuard{expectGeneration: fetchClaim.Generation()},
	)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errPRInfoResultRaced):
		// A newer producer already won; its generation prevents rollback.
		return nil
	default:
		// No result was recorded. Release this claim unless a newer producer won
		// while the guarded write was waiting.
		inst.CancelPRInfoFetch(fetchClaim)
		return err
	}
}

func prInfoEqual(a, b *git.PRInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
