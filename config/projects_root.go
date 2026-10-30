package config

import (
	"fmt"
	"path/filepath"
)

// validateProjectsRoot checks projects_root (W9.10): empty selects the default
// (<AF home>/projects); otherwise an absolute path, or ~/…, on the DAEMON host.
func validateProjectsRoot(_, v string) error {
	if v != "" && !filepath.IsAbs(ExpandTilde(v)) {
		return fmt.Errorf("projects_root must be an absolute path (or start with ~/) on the daemon host, got %q", v)
	}
	return nil
}
