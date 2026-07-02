package zones_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestMain(m *testing.M) {
	// Repo-wide test hygiene (#837/#1056): tripwire the real config.json and
	// sandbox AGENT_FACTORY_HOME. This package is pure and touches neither,
	// but the guards keep that a verified invariant rather than a hope.
	verifyRealConfig := testguard.ConfigTripwire()
	restoreHome := testguard.SandboxHome()
	code := m.Run()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
