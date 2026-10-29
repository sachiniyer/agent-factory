package daemon

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/projectclone"
)

// cloneProject is the clone seam, replaced by tests.
var cloneProject = projectclone.Clone

// cloneFlights coordinates registrations by destination. Its lock covers table
// bookkeeping only; clones of different repositories run concurrently and never
// wait on each other's git child (#6094).
var cloneFlights projectclone.Flights

// projectsRoot is where URL registrations are cloned: the global projects_root
// on this host, or <AF home>/projects when none is configured. Read per call so
// `af config set projects_root` applies to the next clone without a restart.
//
// A config that exists but cannot be loaded REFUSES the registration: falling
// back to the default would put a (possibly private, possibly large) checkout
// somewhere the operator did not choose (#6095). LoadConfig itself answers a
// genuinely absent config with defaults, so only a real failure reaches the
// error branch.
func projectsRoot() (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", fmt.Errorf("cannot read the daemon's config to find projects_root, so nothing was cloned (fix the config, then retry): %w", err)
	}
	if cfg.ProjectsRoot != "" {
		return filepath.Clean(config.ExpandTilde(cfg.ProjectsRoot)), nil
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the af home for the projects root: %w", err)
	}
	return filepath.Join(dir, "projects"), nil
}

// cloneProjectForURL clones url under projects_root and returns the checkout.
// The whole registration, including a wait on an identical clone already in
// flight, is bounded by projectclone.Budget.
func cloneProjectForURL(url string) (string, error) {
	if err := projectclone.Validate(url); err != nil {
		return "", err
	}
	root, err := projectsRoot()
	if err != nil {
		return "", err
	}
	name, err := projectclone.RepoName(url)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), projectclone.Budget)
	defer cancel()
	return cloneFlights.Do(ctx, filepath.Join(root, name), url, func() (string, error) {
		return cloneProject(ctx, root, url)
	})
}
