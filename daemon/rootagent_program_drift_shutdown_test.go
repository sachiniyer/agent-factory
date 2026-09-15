package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

func TestShutdownAbandonsBlockedRootProgramInspection(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())

	entered := make(chan struct{})
	release := make(chan struct{})
	previousResolve := resolveRootProgramConfigForInspection
	resolveRootProgramConfigForInspection = func(*config.RepoContext, *config.Config) (*config.ResolvedConfig, error) {
		close(entered)
		<-release
		return &config.ResolvedConfig{Config: *config.DefaultConfig()}, nil
	}
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		manager.waitRootProgramDriftInspections()
		resolveRootProgramConfigForInspection = previousResolve
	})

	state := &rootEnsureState{programDriftResolving: true, programDriftResolvingEpoch: 1}
	manager.launchAdoptedRootProgramResolution(repo, repo.ID, "root", repoPath, state,
		config.RootAgent{Enabled: true, Program: "codex"}, nil, 1, config.DefaultConfig(), nil,
		session.RuntimeProgramEvidence{})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("root-program inspection did not enter the blocked read")
	}

	stopCh := make(chan struct{})
	var workers sync.WaitGroup
	httpClosed, controlClosed := false, false
	drained := make(chan struct{})
	go func() {
		drainDaemon(manager, nil, func() error { return nil }, &httpClosed, &controlClosed, stopCh, &workers)
		close(drained)
	}()

	select {
	case <-drained:
		// Read-only inspection results have no durable shutdown side effect, so
		// shutdown owes them neither completion nor cancellation.
	case <-time.After(500 * time.Millisecond):
		close(release)
		released = true
		manager.waitRootProgramDriftInspections()
		<-drained
		t.Fatal("daemon shutdown waited for a blocked read-only root-program inspection")
	}

	manager.mu.Lock()
	inFlight := manager.rootProgramDriftInFlight[repoPath]
	manager.mu.Unlock()
	if inFlight != 1 {
		t.Fatalf("blocked root-program inspections after shutdown = %d, want 1 to prove shutdown returned before the read", inFlight)
	}
	close(release)
	released = true
	manager.waitRootProgramDriftInspections()
}
