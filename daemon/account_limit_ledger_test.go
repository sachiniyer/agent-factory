package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

func TestLoadPersistedAccountLimitObservationsFailsClosedOnSkippedRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const repoID = "unreadable-limit-evidence"
	if err := config.SaveRepoInstances(repoID, json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "instances", repoID, config.InstancesFileName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := loadPersistedAccountLimitObservations(); err == nil {
		t.Fatal("automatic selection treated a skipped repo as proof that it held no limit evidence")
	}
}
