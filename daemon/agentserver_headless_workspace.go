package daemon

import "github.com/sachiniyer/agent-factory/session"

// What a stop does to the headless agent-server's workspace (#5571 W8.6). Split
// out of agentserver_headless.go for the file-length limit.

// shutdownWorkspace is what a SIGTERM does to the workspace. An ordinary server
// kills it. A server that adopted a preserved workspace (#5571 W8.6) never does:
// the work in it belongs to the archive, so every stop of that server — a failed
// adoption, an aborted restore, a rollback — stops the agent and tabs and leaves
// the tree where it is. An explicit Kill RPC (the user's kill) is a different
// path and still destroys it.
func (hs *headlessServer) shutdownWorkspace() error {
	if !hs.adopted {
		return hs.as.Kill(false)
	}
	stopper, ok := hs.as.(session.WorkspaceStopper)
	if !ok {
		// No safe stop is available, and Kill is the unsafe one: leave everything.
		return session.ErrPreserveUnsupported
	}
	return stopper.StopPreservingWorkspace()
}
