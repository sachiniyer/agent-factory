package session

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #6258: the agent session of an instance is launched with the instance's id in
// its environment (AF_INSTANCE_ID), the value a cron fire records durably before
// the spawn and a later reconcile matches. Driven through the same
// refreshSessionEnvironment every launch, restore and recover goes through.
func TestRefreshSessionEnvironmentStampsTheInstanceIDIntoTheLaunch(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_instance_id_stamp"
	inner := nameKeyedExec(map[string]bool{})
	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			commands = append(commands, c.String())
			return inner.Run(c)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	ts := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "sh", pty, cmdExec)

	i := &Instance{ID: "inst-6258-launch", Title: "stamp", Program: "sh"}
	require.NoError(t, refreshSessionEnvironment(i, ts))
	require.NoError(t, ts.Start(t.TempDir()))

	var launch string
	for _, c := range commands {
		if strings.Contains(c, "new-session") {
			launch = c
		}
	}
	require.NotEmpty(t, launch, "the agent session must have been launched")
	require.Contains(t, launch, "-e AF_INSTANCE_ID=inst-6258-launch ")
}
