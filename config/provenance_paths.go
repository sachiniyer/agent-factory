package config

import (
	"fmt"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// RebaseProjectPathsForDisplay changes only the spelling of project-scoped
// paths carried by an explanation. Resolution and project identity must use
// the canonical root passed to ResolveConfig; a CLI can call this afterward
// when it wants locations to match the lexical path a user selected.
//
// displayRoot must resolve to the same filesystem location as ProjectRoot.
// Every project-scoped source is validated before anything is changed, so an
// invalid rebase cannot leave an explanation with a mixture of old and new
// paths.
func (r *ResolvedConfig) RebaseProjectPathsForDisplay(displayRoot string) error {
	if r == nil {
		return fmt.Errorf("cannot rebase paths on a nil resolved config")
	}
	if r.ProjectRoot == "" {
		return fmt.Errorf("cannot rebase project paths on a global resolution")
	}
	if !filepath.IsAbs(displayRoot) {
		return fmt.Errorf("project display root must be absolute: %q", displayRoot)
	}

	resolvedRoot := pathutil.ResolveForCompare(r.ProjectRoot)
	displayRoot = filepath.Clean(displayRoot)
	if pathutil.ResolveForCompare(displayRoot) != resolvedRoot {
		return fmt.Errorf("project display root %q does not resolve to project root %q", displayRoot, r.ProjectRoot)
	}

	rebased := make(map[string]string)
	collect := func(layer, path string) error {
		if layer != SourceRepoShared.String() || path == "" {
			return nil
		}
		if _, ok := rebased[path]; ok {
			return nil
		}

		resolvedPath := pathutil.ResolveForCompare(path)
		if resolvedPath != resolvedRoot && !pathutil.IsStrictlyInside(resolvedPath, resolvedRoot) {
			return fmt.Errorf("%s source path %q is outside project root %q", SourceRepoShared, path, r.ProjectRoot)
		}
		rel, err := filepath.Rel(resolvedRoot, resolvedPath)
		if err != nil {
			return fmt.Errorf("rebase project source path %q: %w", path, err)
		}
		rebased[path] = filepath.Join(displayRoot, rel)
		return nil
	}

	var validationErr error
	r.visitSourceLocations(func(layer string, path *string) {
		if validationErr == nil {
			validationErr = collect(layer, *path)
		}
	})
	if validationErr != nil {
		return validationErr
	}

	// The validation pass above populated every non-empty repo-shared path,
	// so this pass cannot fail halfway through and partially mutate the trace.
	r.visitSourceLocations(func(layer string, path *string) {
		if layer == SourceRepoShared.String() && *path != "" {
			*path = rebased[*path]
		}
	})
	r.ProjectRoot = displayRoot
	return nil
}

// visitSourceLocations is the one inventory of filesystem locations in a
// resolution trace. Keeping winner, per-leaf origin, and candidate traversal
// here prevents presentation transforms from updating only part of a trace.
func (r *ResolvedConfig) visitSourceLocations(visit func(layer string, path *string)) {
	for i := range r.Resolution {
		value := &r.Resolution[i]
		if value.Winner != nil {
			visit(value.Winner.Layer, &value.Winner.Path)
		}
		for leaf, origin := range value.Origins {
			visit(origin.Layer, &origin.Path)
			value.Origins[leaf] = origin
		}
		for j := range value.Candidates {
			candidate := &value.Candidates[j]
			visit(candidate.Layer, &candidate.Path)
		}
	}
}
