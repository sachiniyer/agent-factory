package api

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// TestSessionsCreate_RejectsReservedRootTitle covers the CLI side of the
// #1106 name reservation: `af sessions create --name root` (any casing)
// fails fast with an actionable error naming the root_agents opt-in, without
// a daemon round trip. The daemon's reserveCreate stays the authoritative,
// race-safe gate.
func TestSessionsCreate_RejectsReservedRootTitle(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	silenceStdio(t)

	repoRoot := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "init", repoRoot).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		return nil, errors.New("daemon must not be reached for a reserved title")
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	for _, name := range []string{"root", "Root"} {
		setSessionsCreateFlags(t, name, repoRoot, false, false)
		err := sessionsCreateCmd.RunE(sessionsCreateCmd, nil)
		if err == nil {
			t.Fatalf("expected reserved title %q to be rejected", name)
		}
		if !strings.Contains(err.Error(), "reserved") || !strings.Contains(err.Error(), "root_agents") {
			t.Fatalf("rejection for %q must name the reservation and the root_agents opt-in, got: %v", name, err)
		}
	}
}
