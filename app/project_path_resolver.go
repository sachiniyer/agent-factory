package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

const (
	projectPathScanTimeout = 150 * time.Millisecond
	// registeredProjectProveTimeout is deliberately LONGER than the generic path
	// scan: proving a registration runs two Git probes plus a marker read where
	// the generic resolver runs one, and it matches config's own
	// registeredProjectProbeTimeout. An under-budgeted proof is not a neutral
	// failure here — absence of proof downgrades the entry to its recorded-root
	// identity, so a healthy project starved of milliseconds would split into two
	// rows until the cache refreshed. The deadline is only ever spent on
	// UNCACHED entries, behind the same one-second TTL.
	registeredProjectProveTimeout = 250 * time.Millisecond
	projectPathResolutionTTL      = time.Second
)

type projectPathResolution struct {
	id         string
	root       string
	resolvedAt time.Time
	// answeredNotARepo records a DETERMINATE verdict about this path: git said
	// it is not inside a repository, or the path is provably gone. A probe that
	// timed out, could not run, or failed for an operational reason — dubious
	// ownership, an unreadable .git — leaves this false (#3530 review ids
	// 3918120760, 3919195017). resolvedAt alone cannot tell those apart, and a
	// caller that treats any failure as "nothing is there" hands a live
	// repository's root_agents key to a stale registry row.
	answeredNotARepo bool
}

// projectPathProbeOutcome is HOW one path's probe ended, which is a different
// question from what the poll concluded about the path. The distinction exists
// because two of these outcomes produce the identical fallback — a probe git
// answered with an operational failure and a probe that was killed unspent both
// leave the recorded identity in place — so an assertion about the fallback
// cannot tell them apart, and a test meaning to pin the first silently passes
// on the second whenever the runner is slow enough (#3765).
type projectPathProbeOutcome int

const (
	// probeOutcomeUnanswered is the ZERO VALUE on purpose: a budget is
	// unanswered until a result proves otherwise. That is the same inverted
	// discipline config.probeWentUnanswered documents — enumerating the ways a
	// probe can fail to answer is how #3500 happened, and each gap in such a
	// list becomes a fabricated verdict. Here it also covers the budget whose
	// result never reached the gather loop at all, which is unanswered in the
	// most literal sense.
	probeOutcomeUnanswered projectPathProbeOutcome = iota
	// probeOutcomeResolved: git answered with a repository.
	probeOutcomeResolved
	// probeOutcomeNotARepository: git answered that the path is not inside a
	// repository. Deliberately NOT the same as the poll's answeredNotARepo,
	// which a provably-absent path also satisfies without git having said
	// anything — that verdict is about the PATH and this is about the PROBE.
	probeOutcomeNotARepository
	// probeOutcomeFailed: git ran and exited without a verdict — dubious
	// ownership, an unreadable .git, output it could not parse.
	probeOutcomeFailed
)

func (o projectPathProbeOutcome) String() string {
	switch o {
	case probeOutcomeResolved:
		return "resolved"
	case probeOutcomeNotARepository:
		return "answered: not a repository"
	case probeOutcomeFailed:
		return "git ran and failed without a verdict"
	default:
		return "unanswered: the probe did not complete"
	}
}

// classifyProbeOutcome reads how the probe ended off the ERROR, using config's
// shared classifier rather than a second copy of the rule (#3504). Two
// properties of that classifier are what make this sound: an answer is proven
// only by a completed exit status, and it deliberately never consults ctx.Err(),
// so a budget expiring in the window just after git exited 128 cannot retag
// git's answer as a cancellation (#3500 review).
func classifyProbeOutcome(err error) projectPathProbeOutcome {
	switch {
	case err == nil:
		return probeOutcomeResolved
	case errors.Is(err, config.ErrNotGitRepository):
		return probeOutcomeNotARepository
	case config.RepoProbeUnanswered(err):
		return probeOutcomeUnanswered
	default:
		return probeOutcomeFailed
	}
}

type projectPathProbeResult struct {
	path       string
	resolution projectPathResolution
	resolved   bool
	// answered marks a failed probe that Git nevertheless answered.
	answered bool
	// outcome is how the probe ENDED, which answered cannot express: answered
	// is a verdict about the path (and a provably-absent path sets it with no
	// help from Git), while this says whether Git ran at all.
	outcome projectPathProbeOutcome
}

// projectPathProbeBudget is ONE resolution budget a poll opened: the path it was
// opened for, and the whole window that path's probe was given to answer in.
//
// Budgets are per PATH and never per poll (see resolveProjectPaths), and this
// record is what lets that independence be asserted as a PROPERTY rather than
// inferred from how long a poll took. It was inferred from elapsed time until
// #3710: a test stalled one inactive path and then checked that a healthy
// worktree had still resolved inside the first poll — which holds only when the
// healthy probe's own Git work finishes inside the window on whatever machine
// the test lands on. A loaded runner misses that while the property it stood in
// for still holds, and master's release Build went red for exactly that.
type projectPathProbeBudget struct {
	path   string
	window time.Duration
	// outcome is filled in when the probe's result reaches the gather loop, and
	// stays probeOutcomeUnanswered when none ever does.
	outcome projectPathProbeOutcome
}

// resolveProjectPaths gives every uncached path a bounded resolution
// opportunity of its OWN: each probe is opened with the whole scan window, and
// the probes run together rather than in turn. An unreachable checkout
// therefore spends only its own budget — a healthy linked worktree later in the
// list is neither queued behind it nor handed whatever it left.
// Successful answers are cached briefly to avoid probing on every 750ms sidebar
// poll, then revalidated so a present path replaced by another checkout cannot
// retain its former repository identity forever.
//
// The second return is the ledger of budgets opened, one entry per uncached
// path, in the order they were opened. Production ignores it; it exists so the
// independence above can be asserted without a stopwatch (#3710).
func (m *home) resolveProjectPaths(paths []string) (map[string]projectPathResolution, []projectPathProbeBudget) {
	now := time.Now()
	resolvedPaths := make(map[string]projectPathResolution, len(paths))
	if m.repoRoot != "" && m.repoID != "" {
		// Opening the home already established this identity. It remains the
		// authoritative active spelling even while unrelated probes time out.
		resolvedPaths[m.repoRoot] = projectPathResolution{id: m.repoID, root: m.repoRoot}
	}

	uncached := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := resolvedPaths[path]; ok {
			continue
		}
		if cached, ok := m.projectPathResolutions[path]; ok && now.Sub(cached.resolvedAt) < projectPathResolutionTTL {
			resolvedPaths[path] = cached
			continue
		}
		delete(m.projectPathResolutions, path)
		resolvedPaths[path] = projectPathResolution{
			id: config.RepoIDFromRoot(filepath.Clean(path)), root: filepath.Clean(path),
		}
		uncached = append(uncached, path)
	}
	if len(uncached) == 0 {
		return resolvedPaths, nil
	}

	// One window per path, opened at the probe that spends it. Sharing a single
	// window across the paths is what this must not do: whichever path the poll
	// reached first would spend it, and every path after it would be probed with
	// the remainder — which is nothing at all once one of them is stalled.
	budgets := make([]projectPathProbeBudget, 0, len(uncached))
	// Every path in uncached is distinct (a path already in resolvedPaths is
	// skipped above), so one index per path is enough to route each result back
	// to the budget it was granted under.
	budgetIndex := make(map[string]int, len(uncached))
	cancels := make([]context.CancelFunc, 0, len(uncached))
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	results := make(chan projectPathProbeResult, len(uncached))
	for _, path := range uncached {
		ctx, cancel := context.WithTimeout(context.Background(), projectPathScanTimeout)
		cancels = append(cancels, cancel)
		budgetIndex[path] = len(budgets)
		budgets = append(budgets, projectPathProbeBudget{path: path, window: projectPathScanTimeout})
		go func(ctx context.Context, path string) {
			repo, err := config.RepoFromPathContext(ctx, path)
			if err != nil {
				// A DETERMINATE verdict is what a caller may act on, and
				// "the probe failed" is not one (#3530 review ids 3918120760,
				// 3919195017). Two outcomes qualify: git answered that the
				// path is not inside a repository, or the path is provably
				// gone. Everything else — a killed probe, dubious ownership,
				// an unreadable .git, a permission error — leaves a repository
				// that may well be there, and the daemon will apply a
				// path-keyed opt-in to whatever IS there.
				answered := errors.Is(err, config.ErrNotGitRepository)
				if !answered {
					if _, statErr := os.Stat(path); statErr != nil && config.PathDeterminatelyAbsent(statErr) {
						answered = true
					}
				}
				results <- projectPathProbeResult{
					path:       path,
					answered:   answered,
					resolved:   false,
					resolution: projectPathResolution{},
					outcome:    classifyProbeOutcome(err),
				}
				return
			}
			results <- projectPathProbeResult{
				path: path,
				resolution: projectPathResolution{
					id: repo.ID, root: repo.WorkspacePath(), resolvedAt: time.Now(),
				},
				resolved: true,
				outcome:  probeOutcomeResolved,
			}
		}(ctx, path)
	}
	// The gather's deadline bounds a different thing from the budgets above, and
	// hands out no opportunity that a path could be competing for: every probe
	// cancels its own Git, but a stall BELOW Git — an unresponsive mount, or a
	// killed process whose child still holds the pipe — outlives that kill, so
	// the loop has to stop waiting on a clock of its own. It is started after
	// the probes so it can only ever expire later than their windows: a probe
	// still inside its own budget is never cut short by it.
	gather, cancelGather := context.WithTimeout(context.Background(), projectPathScanTimeout)
	defer cancelGather()
	if m.projectPathResolutions == nil {
		m.projectPathResolutions = make(map[string]projectPathResolution)
	}
	for range uncached {
		select {
		case result := <-results:
			if index, ok := budgetIndex[result.path]; ok {
				budgets[index].outcome = result.outcome
			}
			if result.resolved {
				resolvedPaths[result.path] = result.resolution
				m.projectPathResolutions[result.path] = result.resolution
				continue
			}
			if result.answered {
				// Keep the fallback identity, and record that the negative is
				// a VERDICT: the caller may act on "nothing is there" only
				// when Git said so. Deliberately not cached — an absent path
				// is exactly what comes back.
				fallback := resolvedPaths[result.path]
				fallback.answeredNotARepo = true
				resolvedPaths[result.path] = fallback
			}
		case <-gather.Done():
			return resolvedPaths, budgets
		}
	}
	return resolvedPaths, budgets
}

// registeredProjectIdentity is a registry entry's PROVEN repository identity —
// proven in the ResolveRegisteredProjectRepoID sense: Git still recognizes that
// exact workspace and its checkout marker still matches the registration.
type registeredProjectIdentity struct {
	id         string
	resolvedAt time.Time
}

type registeredProjectProbeResult struct {
	root     string
	id       string
	resolved bool
}

// resolveRegisteredProjectIdentities proves each registered root's identity
// before the switcher is willing to treat the registration as evidence about a
// repository.
//
// A durable registration names a path, and a path is not identity. If a nested
// checkout loses its .git metadata while its directory survives inside an outer
// repository, or another checkout takes over the path, the generic resolver
// happily answers with the ENCLOSING or REPLACEMENT repository — and the
// switcher would then add or merge a Projects row for it. Switching there opens
// a repository the user never registered, and deleting the row aims
// delete-project at that repository's sessions and root-agent state.
//
// config.ResolveRegisteredProjectRepoID is the same exact-workspace plus
// checkout-marker check delete-project already applies, so the picker and the
// destructive verb cannot disagree about which registrations are real.
//
// Failure is not exclusion: an unproven entry falls back to its RECORDED root
// identity at the call site, so a registered project on a stalled mount stays
// visible under its own hash instead of vanishing or borrowing an ancestor's.
// Each registration is given a proving budget of its OWN and they run together,
// exactly as resolveProjectPaths does: one unreachable registration must not
// spend an opportunity that belongs to another.
func (m *home) resolveRegisteredProjectIdentities(projects []config.Project) map[string]string {
	now := time.Now()
	proven := make(map[string]string, len(projects))
	uncached := make([]config.Project, 0, len(projects))
	for _, project := range projects {
		if project.Root == "" {
			continue
		}
		if !project.PathExists {
			// Absence outranks the short TTL: drop the cached proof at once so a
			// vanished checkout cannot keep vouching for itself for another second.
			delete(m.registeredProjectIdentities, project.Root)
			continue
		}
		if cached, ok := m.registeredProjectIdentities[project.Root]; ok && now.Sub(cached.resolvedAt) < projectPathResolutionTTL {
			proven[project.Root] = cached.id
			continue
		}
		delete(m.registeredProjectIdentities, project.Root)
		uncached = append(uncached, project)
	}
	if len(uncached) == 0 {
		return proven
	}

	cancels := make([]context.CancelFunc, 0, len(uncached))
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	results := make(chan registeredProjectProbeResult, len(uncached))
	for _, project := range uncached {
		ctx, cancel := context.WithTimeout(context.Background(), registeredProjectProveTimeout)
		cancels = append(cancels, cancel)
		go func(ctx context.Context, project config.Project) {
			id, ok := config.ResolveRegisteredProjectRepoID(ctx, project)
			results <- registeredProjectProbeResult{root: project.Root, id: id, resolved: ok}
		}(ctx, project)
	}
	// The backstop for the gather, not a budget any registration competes for —
	// see resolveProjectPaths for why the loop cannot wait on the probes' own
	// deadlines, and why this one is started after them.
	gather, cancelGather := context.WithTimeout(context.Background(), registeredProjectProveTimeout)
	defer cancelGather()
	if m.registeredProjectIdentities == nil {
		m.registeredProjectIdentities = make(map[string]registeredProjectIdentity)
	}
	for range uncached {
		select {
		case result := <-results:
			if result.resolved {
				proven[result.root] = result.id
				m.registeredProjectIdentities[result.root] = registeredProjectIdentity{
					id: result.id, resolvedAt: time.Now(),
				}
			}
		case <-gather.Done():
			return proven
		}
	}
	return proven
}
