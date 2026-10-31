package ui

import (
	"strings"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
)

// isRemoteTaskTarget is a seam so the ui tests can exercise the remote branch
// without a daemon URL in the environment.
var isRemoteTaskTarget = apiclient.IsRemoteTarget

// ResolveTaskProjectPath normalizes the project path typed into a task form.
//
// Locally it is ExpandTilde + filepath.Abs, as it has always been. Against a
// remote target (W9.7) the path names a checkout on the DAEMON'S host, so this
// machine's home and working directory must not touch it: it is checked only for
// shape (absolute, or starting with ~/), kept AS TYPED so the daemon expands ~
// against its own home, and whether it is a git repository is the daemon's call
// when it takes the task.
func ResolveTaskProjectPath(raw string) (string, error) {
	if !isRemoteTaskTarget() {
		return config.ResolveUserPath(raw)
	}
	trimmed := strings.TrimSpace(raw)
	if _, err := config.ResolveDaemonHostPath(trimmed); err != nil {
		return "", err
	}
	return trimmed, nil
}
