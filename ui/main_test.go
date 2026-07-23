package ui

import (
	"fmt"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestMain(m *testing.M) {
	sessionenv.HandleInternalExec()
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: fail loudly if a test leaks an af_ session onto the ambient tmux
	// server (preview tests issue real tmux kill-session commands).
	verifyTmux := testguard.TmuxTripwire()
	// #1056: default the whole package into a sandboxed AGENT_FACTORY_HOME.
	// Many tests here call log.Initialize without setting a home of their
	// own; the sandbox routes those log files (and any stray config/state
	// writes) into a temp dir instead of the developer's real one.
	restoreHome := testguard.SandboxHome()
	// #1122: default the whole package onto a private tmux server so a test
	// that forgets IsolateTmux can never create or sweep sessions on the
	// developer's real server.
	restoreTmux := testguard.SandboxTmux()
	code := m.Run()
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
