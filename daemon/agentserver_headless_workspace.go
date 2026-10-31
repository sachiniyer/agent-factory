package daemon

import (
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

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

// StopPreserving stops the agent and its tabs and leaves the workspace on disk
// (#5571 W8.5): archive without publishing. The process then exits, since nothing
// is left for it to serve; its shutdown path kills a workspace it no longer holds,
// which is a no-op.
func (hs *headlessServer) StopPreserving(_ struct{}, resp *agentOKResponse) error {
	stopper, ok := hs.as.(session.WorkspaceStopper)
	if !ok {
		return session.ErrPreserveUnsupported
	}
	if err := stopper.StopPreservingWorkspace(); err != nil {
		return err
	}
	resp.OK = true
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
	return nil
}

// Relaunch stops the agent and starts it again in this workspace. Nothing is
// pushed, reaped or re-cloned: the branch, tree and uncommitted work stay as
// they are.
func (hs *headlessServer) Relaunch(_ struct{}, resp *agentOKResponse) error {
	r, ok := hs.as.(session.AgentRelauncher)
	if !ok {
		return session.ErrRelaunchUnsupported
	}
	if err := r.Relaunch(); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

type agentSwapRequest struct {
	Target string `json:"target"`
}

// agentSwapResponse carries the command the host resolved and launched, which the
// driving daemon records as the new runtime's evidence.
type agentSwapResponse struct {
	OK      bool   `json:"ok"`
	Program string `json:"program"`
}

// SwapAgent replaces the agent with another in this workspace. The branch, tree
// and uncommitted work stay as they are; nothing is pushed, reaped or re-cloned.
func (hs *headlessServer) SwapAgent(req agentSwapRequest, resp *agentSwapResponse) error {
	sw, ok := hs.as.(session.AgentSwapper)
	if !ok {
		return session.ErrHandoffUnsupported
	}
	program, err := sw.SwapAgentTo(req.Target)
	if err != nil {
		return err
	}
	resp.OK = true
	resp.Program = program
	return nil
}

// launchAdopted starts the agent in the worktree this server adopted (#5571 W8.6),
// resuming the recorded conversation. Nothing is created and no hook runs.
func (hs *headlessServer) launchAdopted(req agentLifecycleRequest, resp *agentOKResponse) error {
	if err := hs.inst.LaunchAdopted(req.Conversation); err != nil {
		return err
	}
	resp.OK = true
	if conv := hs.inst.AgentConversation(); conv.HasID() {
		resp.Conversation = &conv
	}
	return nil
}

// restoreSurvivingTabs is the rebind, a seam so the launch wrapper's wiring is
// testable without a tmux server.
var restoreSurvivingTabs = func(inst *session.Instance, rows []session.TabData) session.TabRestoreReport {
	return inst.RestoreSurvivingTabs(rows)
}

// Launch runs the launch and then, for a restart adoption, rebinds the surviving
// shell/process tabs (W1.4, #6162). It does so after EITHER launch path: a sandbox
// created normally restarts through its persisted launch script with no
// --adopt-worktree, so hs.adopted is false there and only this wrapper sees it.
func (hs *headlessServer) Launch(req agentLifecycleRequest, resp *agentOKResponse) error {
	if err := hs.launchWorkspace(req, resp); err != nil {
		return err
	}
	if req.RestartAdoption && len(req.Tabs) > 0 && hs.inst != nil {
		rep := restoreSurvivingTabs(hs.inst, req.Tabs)
		resp.TabsRestored, resp.TabsGone = rep.Restored, rep.Gone
	}
	return nil
}

// RestoreTabs rebinds surviving tabs on a running server, outside a launch: the
// daemon's retry for tabs a restart adoption left unknown (#6235, #6236). It is
// idempotent — an id already held is reported by its true state.
func (hs *headlessServer) RestoreTabs(req agentLifecycleRequest, resp *agentOKResponse) error {
	if hs.inst == nil {
		return errors.New("agent-server has no workspace to restore tabs into")
	}
	rep := restoreSurvivingTabs(hs.inst, req.Tabs)
	resp.OK = true
	resp.TabsRestored, resp.TabsGone = rep.Restored, rep.Gone
	return nil
}
