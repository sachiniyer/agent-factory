package zones_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestMain(m *testing.M) {
	// Repo-wide test hygiene (#837/#1056/#4469): tripwire the real config.json
	// and the real ~/.codex store, and sandbox AGENT_FACTORY_HOME. This
	// package is pure and touches none of them, but the guards keep that a
	// verified invariant rather than a hope.
	verifyRealConfig := testguard.ConfigTripwire()
	verifyCodex := testguard.CodexHomeTripwire()
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
