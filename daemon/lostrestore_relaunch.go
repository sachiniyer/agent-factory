package daemon

import "github.com/sachiniyer/agent-factory/session"

// preserveUnlessRelaunchable is the pre-replacement preserve push, skipped when
// the sandbox can relaunch its agent in place (#5571 W7.1). That relaunch
// replaces nothing — the workspace, its branch and any uncommitted work stay
// where they are — so there is nothing to make durable first, and pushing would
// publish a branch for a recovery that never leaves the sandbox. The push still
// runs for every runtime that will be re-provisioned (an older sandbox build),
// exactly as before.
func (m *Manager) preserveUnlessRelaunchable(repoID, key string, inst *session.Instance) error {
	if inst.CanRelaunchInPlace() {
		return nil
	}
	return m.preserveSandboxBeforeReap(repoID, key, inst, reapRefusalSuggestionFor(repoID, inst))
}

// replacesSandbox reports whether a respawn of instance re-provisions a remote
// sandbox — and so owes the preserve push and a durable branch first. A sandbox
// that relaunches in place (W7.1) replaces nothing and owes neither.
func replacesSandbox(instance *session.Instance) bool {
	return isRemoteWorkspace(instance) && !instance.CanRelaunchInPlace()
}
