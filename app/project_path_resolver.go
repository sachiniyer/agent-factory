package app

import (
	"context"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

const (
	projectPathScanTimeout   = 150 * time.Millisecond
	projectPathResolutionTTL = time.Second
)

type projectPathResolution struct {
	id         string
	root       string
	resolvedAt time.Time
}

type projectPathProbeResult struct {
	path       string
	resolution projectPathResolution
	resolved   bool
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
			id: config.RepoIDForRecordedRoot(path), root: filepath.Clean(path),
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
				results <- projectPathProbeResult{path: path}
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
			}
		case <-ctx.Done():
			return resolvedPaths
		}
	}
	return resolvedPaths
}
