package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/projectclone"
)

// cloneProject is the clone seam, replaced by tests.
var cloneProject = projectclone.Clone

// cloneMu serializes clones: two registrations of the same URL must not race to
// create one directory, and a clone is rare enough that a queue costs nothing.
var cloneMu sync.Mutex

// projectsRoot is where URL registrations are cloned: the global projects_root
// on this host, or <AF home>/projects. Read per call so `af config set
// projects_root` applies to the next clone without a restart.
func projectsRoot() (string, error) {
	if cfg, err := config.LoadConfig(); err == nil && cfg.ProjectsRoot != "" {
		return filepath.Clean(config.ExpandTilde(cfg.ProjectsRoot)), nil
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the af home for the projects root: %w", err)
	}
	return filepath.Join(dir, "projects"), nil
}

// cloneProjectForURL clones url under projects_root and returns the checkout.
func cloneProjectForURL(url string) (string, error) {
	if err := projectclone.Validate(url); err != nil {
		return "", err
	}
	root, err := projectsRoot()
	if err != nil {
		return "", err
	}
	cloneMu.Lock()
	defer cloneMu.Unlock()
	return cloneProject(context.Background(), root, url)
}
