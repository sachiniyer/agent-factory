package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The sweep never runs on an incomplete picture of what is owned.
func TestSandboxDirSweepSkipsWhileAnyRepoIsSkipped(t *testing.T) {
	m := &Manager{skippedRepos: []SkippedRepo{{RepoID: "corrupt"}}}
	called := make(chan struct{}, 1)
	prev := sweepSandboxOrphans
	sweepSandboxOrphans = func([]session.SweepRow, time.Duration, func(string, string) bool) []session.SweepFinding {
		called <- struct{}{}
		return nil
	}
	t.Cleanup(func() { sweepSandboxOrphans = prev })

	m.sweepStartupOrphanSandboxDirs()
	select {
	case <-called:
		t.Fatal("the sweep ran with an unloadable repo: the owned set would be incomplete")
	case <-time.After(200 * time.Millisecond):
	}

	m.skippedRepos = nil
	m.sweepStartupOrphanSandboxDirs()
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep should run once every repo loaded")
	}
}
