package integration_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestMain guards the package with the #837 config tripwire. The tests here
// build and exec real af binaries that spawn daemons, so they are the most
// likely place for a sandbox escape to reach the developer's real
// ~/.agent-factory/config.json.
func TestMain(m *testing.M) {
	verifyRealConfig := testguard.ConfigTripwire()
	code := m.Run()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
