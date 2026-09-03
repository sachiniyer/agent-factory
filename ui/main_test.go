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
	// #3708: the config pane's read and save now follow the remote daemon target,
	// so an AF_DAEMON_URL inherited from the developer's shell would silently
	// point this package's config tests at someone else's daemon — and the local
	// ones would then fail for a reason nothing on screen explains. Clear it for
	// the package run, in the same spirit as the two sandboxes above. The tests
	// that WANT a remote target set it themselves (config_target_test.go).
	_ = os.Unsetenv("AF_DAEMON_URL")
	_ = os.Unsetenv("AF_DAEMON_TOKEN")
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
