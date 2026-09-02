package app

import (
	"context"
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
	// answeredNotARepo records that Git ANSWERED about this path and the
	// answer was "not a repository" — as against a probe that timed out or
	// could not run, which leaves this false (#3530 review id 3918120760).
	// resolvedAt alone cannot tell those apart, and a caller that treats an
	// unanswered probe as "nothing is there" hands a live repository's
	// root_agents key to a stale registry row.
	answeredNotARepo bool
}

type projectPathProbeResult struct {
	path       string
	resolution projectPathResolution
	resolved   bool
	// answered marks a failed probe that Git nevertheless answered.
	answered bool
}

// resolveProjectPaths gives every uncached path the same bounded wall-clock
// opportunity. Probes start together: an unreachable checkout cannot exhaust a
// shared sequential deadline before a later healthy linked worktree is tried.
// Successful answers are cached briefly to avoid probing on every 750ms sidebar
// poll, then revalidated so a present path replaced by another checkout cannot
// retain its former repository identity forever.
func (m *home) resolveProjectPaths(paths []string) map[string]projectPathResolution {
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
		return resolvedPaths
	}

	ctx, cancel := context.WithTimeout(context.Background(), projectPathScanTimeout)
	defer cancel()
	results := make(chan projectPathProbeResult, len(uncached))
	for _, path := range uncached {
		go func(path string) {
			repo, err := config.RepoFromPathContext(ctx, path)
			if err != nil {
				// An ANSWER is what a caller may act on; a killed or
				// abandoned probe is not one. config owns that rule.
				results <- projectPathProbeResult{
					path:       path,
					answered:   !config.RepoProbeUnanswered(err),
					resolved:   false,
					resolution: projectPathResolution{},
				}
				return
			}
			results <- projectPathProbeResult{
				path: path,
				resolution: projectPathResolution{
					id: repo.ID, root: repo.WorkspacePath(), resolvedAt: time.Now(),
				},
				resolved: true,
			}
		}(path)
	}
	if m.projectPathResolutions == nil {
		m.projectPathResolutions = make(map[string]projectPathResolution)
	}
	for range uncached {
		select {
		case result := <-results:
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
		case <-ctx.Done():
			return resolvedPaths
		}
	}
	return resolvedPaths
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
// Probes start together and share one deadline, matching resolveProjectPaths:
// one unreachable registration must not spend the whole poll's opportunity.
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

	ctx, cancel := context.WithTimeout(context.Background(), registeredProjectProveTimeout)
	defer cancel()
	results := make(chan registeredProjectProbeResult, len(uncached))
	for _, project := range uncached {
		go func(project config.Project) {
			id, ok := config.ResolveRegisteredProjectRepoID(ctx, project)
			results <- registeredProjectProbeResult{root: project.Root, id: id, resolved: ok}
		}(project)
	}
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
		case <-ctx.Done():
			return proven
		}
	}
	return proven
}
