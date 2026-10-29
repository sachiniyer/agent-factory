package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/session/git"
)

// validateTaskProjectPath is the daemon-side half of a task form's "is this a
// repository?" check (#5859). A remote client cannot make it — the path names a
// directory on THIS host — and the store deliberately accepts unresolved paths,
// so without it a typo'd path saves "successfully", binds to no project, drops
// out of every project's list, and only fails when the task first fires. It
// refuses at create/edit time with a message the user can act on.
//
// path is already ~-expanded. Opt-in (ValidatePath) so callers that validated on
// their own machine, and the legacy rows the store still carries, are unchanged.
func validateTaskProjectPath(path string) error {
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("project path %q must be absolute (or start with ~/): the daemon resolves it on its own filesystem and has no access to your working directory", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("project path %s does not exist on the daemon host (%v); give the path of a git checkout on that machine", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("project path %s is not a directory on the daemon host; give the path of a git checkout", path)
	}
	if err := git.CheckGitRepo(path); err != nil {
		return fmt.Errorf("project path %s is not a git repository on the daemon host (%v); give the path of a git checkout, or register it first with af projects add", path, err)
	}
	return nil
}
