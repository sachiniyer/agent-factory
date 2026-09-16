package termpane

import (
	"fmt"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestMain(m *testing.M) {
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #4469: fail loudly if a test-owned Codex process writes into the real
	// ~/.codex store.
	verifyCodex := testguard.CodexHomeTripwire()
	// #1056: default the whole package into a sandboxed AGENT_FACTORY_HOME so
	// stray log/config/state writes land in a temp dir, mirroring ui/main_test.go.
	restoreHome := testguard.SandboxHome()
	code := m.Run()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := verifyCodex(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
