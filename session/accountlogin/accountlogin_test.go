package accountlogin

import (
	"fmt"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestMain installs the SAME account lookup main.go does before the pane shim
// can consume this test binary, so a login pane spawned here crosses the real
// registry-backed child boundary rather than a test-only shortcut. Without it
// the shim would refuse every scoped launch (a nil lookup fails closed, #3051)
// and the environment assertions below would pass or fail for the wrong reason.
func TestMain(m *testing.M) {
	sessionenv.AccountLookup = func(agent, name string) (sessionenv.Account, error) {
		executable, _ := os.Executable()
		return agentaccount.Selected(os.Getenv("AGENT_FACTORY_HOME"), agent, name, executable)
	}
	sessionenv.HandleInternalExec()
	tmux.HandleDedicatedServerExec()
	verifyRealConfig := testguard.ConfigTripwire()
	verifyTmux := testguard.TmuxTripwire()
	restoreHome := testguard.SandboxHome()
	restoreTmux := testguard.SandboxTmux()
	aflog.Initialize(false)
	code := m.Run()
	aflog.Close()
	restoreTmux()
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
