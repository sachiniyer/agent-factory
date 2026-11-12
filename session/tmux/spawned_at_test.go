package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// timedPtyFactory records when the pane process was spawned.
type timedPtyFactory struct {
	*MockPtyFactory
	spawned time.Time
}

func (f *timedPtyFactory) Start(cmd *exec.Cmd) (*os.File, error) {
	if f.spawned.IsZero() {
		f.spawned = time.Now()
	}
	return f.MockPtyFactory.Start(cmd)
}

// TestStartRecordsTheSpawnInstantAfterTheExistenceGate pins SpawnedAt
// (#7147): a launch dates the replacement of the runtime an operator repair
// was made against by it, so it must be taken after the existence gate
// answered that no session stood behind the name — by then any earlier pane
// under the name was gone, and Restore's own probe runs earlier still — and
// no later than the pane process is spawned.
func TestStartRecordsTheSpawnInstantAfterTheExistenceGate(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	forceSessionEnvExecutable(t, "/test/af")
	forceAgentOnSearchPath(t, "claude")
	ptys := &timedPtyFactory{MockPtyFactory: NewMockPtyFactory(t)}

	var gateAnswered time.Time
	created := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") && !created {
				created = true
				gateAnswered = time.Now()
				return fmt.Errorf("session not found")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if len(cmd.Args) >= 2 && cmd.Args[1] == "show-options" {
				return nil, fmt.Errorf("no server running")
			}
			return []byte("output"), nil
		},
	}
	session := newTmuxSession(toTmuxName("spawned-at", ""), "claude", ptys, cmdExec)
	require.True(t, session.SpawnedAt().IsZero(), "nothing has been spawned yet")

	require.NoError(t, session.Start(t.TempDir()))
	spawnedAt := session.SpawnedAt()
	require.False(t, gateAnswered.IsZero(), "fixture: the existence gate must have run")
	require.False(t, ptys.spawned.IsZero(), "fixture: the pane process must have been spawned")
	require.False(t, spawnedAt.Before(gateAnswered),
		"SpawnedAt %s is before the existence gate answered (%s): a pane still running then could write after it", spawnedAt, gateAnswered)
	require.False(t, spawnedAt.After(ptys.spawned),
		"SpawnedAt %s is after the pane process was spawned (%s): the new pane's first writes would be the old runtime's", spawnedAt, ptys.spawned)
}
