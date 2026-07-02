package integration_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestMain guards the package with the #837 config tripwire and the #1056
// tmux tripwire. The tests here build and exec real af binaries that spawn
// daemons and real tmux sessions, so they are the most likely place for a
// sandbox escape to reach the developer's real ~/.agent-factory/config.json
// or leave orphaned af_ sessions on the developer's tmux server.
func TestMain(m *testing.M) {
	verifyRealConfig := testguard.ConfigTripwire()
	verifyTmux := testguard.TmuxTripwire()
	// #1056: default the package into a sandboxed AGENT_FACTORY_HOME so any
	// in-process config/state/log access outside a per-test home stays out
	// of the developer's real one.
	restoreHome := testguard.SandboxHome()
	code := m.Run()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := verifyTmux(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
