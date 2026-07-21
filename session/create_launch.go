package session

import "fmt"

// CreateLaunchPlan freezes the provider-local facts that must be decided after
// provisioning fixes the final workspace path but before the agent process is
// spawned. Its fields are private so a caller cannot retarget a capture or swap
// the checked command between the two halves.
type CreateLaunchPlan struct {
	instance            *Instance
	launcher            Backend
	prepared            bool
	program             string
	workDir             string
	conversation        AgentConversationData
	conversationCapture ConversationCaptureSnapshot
}

// ConversationCapture returns the provider-store before-image frozen before
// process launch. The daemon passes it unchanged to generation-scoped capture.
func (p CreateLaunchPlan) ConversationCapture() ConversationCaptureSnapshot {
	return p.conversationCapture
}

// preparedCreateBackend is the optional exact-launch boundary implemented by
// the real local backend. Test and off-box backends retain their established
// Start behavior; an off-box process has no daemon-local transcript store to
// capture.
type preparedCreateBackend interface {
	prepareCreateLaunch(instance *Instance) (CreateLaunchPlan, error)
	launchPreparedCreate(instance *Instance, plan CreateLaunchPlan) error
}

// PrepareCreateLaunch snapshots the create's launch context without spawning
// its agent. A backend with an exact prepared boundary is provisioned first so
// relative paths resolve against the final workspace path. Other local test
// backends preserve the legacy daemon-environment snapshot and Start path.
func (i *Instance) PrepareCreateLaunch() (CreateLaunchPlan, error) {
	backend := i.currentBackend()
	plan := CreateLaunchPlan{instance: i, launcher: backend}
	prepared, ok := backend.(preparedCreateBackend)
	if !ok {
		if backend.Capabilities().Workspace == WorkspaceLocalWorktree {
			plan.conversationCapture = BeginConversationCapture()
		}
		return plan, nil
	}
	if err := backend.Provision(i, true); err != nil {
		return CreateLaunchPlan{}, err
	}
	plan, err := prepared.prepareCreateLaunch(i)
	if err != nil {
		return CreateLaunchPlan{}, err
	}
	plan.instance = i
	plan.launcher = backend
	plan.prepared = true
	return plan, nil
}

// LaunchPreparedCreate consumes exactly the plan returned above. The instance
// pointer fence prevents a valid plan from being replayed against another row.
func (i *Instance) LaunchPreparedCreate(plan CreateLaunchPlan) error {
	if plan.instance != i || plan.launcher == nil {
		return fmt.Errorf("create launch plan does not belong to this instance")
	}
	if !plan.prepared {
		return plan.launcher.Start(i, true)
	}
	prepared, ok := plan.launcher.(preparedCreateBackend)
	if !ok {
		return fmt.Errorf("create launch backend no longer supports its prepared plan")
	}
	return prepared.launchPreparedCreate(i, plan)
}
