package ui

import (
	"fmt"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestMain(m *testing.M) {
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: default the whole package into a sandboxed AGENT_FACTORY_HOME.
	// Many tests here call log.Initialize without setting a home of their
	// own; the sandbox routes those log files (and any stray config/state
	// writes) into a temp dir instead of the developer's real one.
	restoreHome := testguard.SandboxHome()
	code := m.Run()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
