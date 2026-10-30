package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAgentServerCommandsCarryTheDaemonNamedBranch pins #5571 W2.4: both
// off-box launchers hand the agent-server the branch the daemon chose, and
// pass nothing when the spec carries none.
func TestAgentServerCommandsCarryTheDaemonNamedBranch(t *testing.T) {
	spec := ProvisionSpec{Title: "remote", Branch: "team/remote"}
	docker, err := (&dockerProvisioner{spec: spec}).agentServerCommand()
	assert.NoError(t, err)
	ssh, err := (&sandboxWorkspace{spec: spec, SessionDir: "/srv/af-session"}).agentServerCommand()
	assert.NoError(t, err)
	for name, cmd := range map[string]string{"docker": docker, "ssh": ssh} {
		assert.Contains(t, cmd, "--branch team/remote", name)
	}

	spec.Branch = ""
	docker, _ = (&dockerProvisioner{spec: spec}).agentServerCommand()
	assert.False(t, strings.Contains(docker, "--branch"), "no branch named, none passed")
}

// TestDaemonBranchForTitleUsesTheFrozenPrefix: the branch comes from the
// snapshot on the options, not from whatever is on disk now.
func TestDaemonBranchForTitleUsesTheFrozenPrefix(t *testing.T) {
	prefix := "frozen/"
	assert.Equal(t, "frozen/my-task", daemonBranchForTitle(InstanceOptions{Title: "my-task", BranchPrefix: &prefix}))
	empty := ""
	assert.Equal(t, "my-task", daemonBranchForTitle(InstanceOptions{Title: "my-task", BranchPrefix: &empty}))
}
