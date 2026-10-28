//go:build !windows

package sessionenv

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAgentServerArgv0KeepsTheAbsoluteSessionSpelling(t *testing.T) {
	prev := os.Args
	t.Cleanup(func() { os.Args = prev })

	os.Args = []string{"/home/u/.af/sessions/s/af", "agent-server"}
	assert.Equal(t, "/home/u/.af/sessions/s/af", agentServerArgv0("/home/u/.af/bin/af-cache"))

	os.Args = []string{"af", "agent-server"}
	assert.Equal(t, "/home/u/.af/bin/af-cache", agentServerArgv0("/home/u/.af/bin/af-cache"))
}
