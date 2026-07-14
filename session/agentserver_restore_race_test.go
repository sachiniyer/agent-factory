package session

import (
	"sync"
	"testing"
)

// TestAgentServer_RestoreRace_NoDataRace is the -race regression for #1729: the
// daemon poll calls Instance.AgentServer() (which reads remoteClient/
// runtimeTeardown to pick the impl) concurrently with a restore/recover that
// rebinds those fields via bindProvisionResult. Before the fix AgentServer() read
// remoteClient under agentSrvMu while bindProvisionResult wrote it under i.mu —
// two unrelated mutexes, a Go-memory-model data race. `go test -race` flags it on
// the pre-fix code and passes clean after AgentServer() moved the read under i.mu.
//
// It is written to hit the read even on a cache miss: bindProvisionResult clears
// the agentSrv cache every iteration, so AgentServer() re-enters its build branch
// (the only place the pre-fix code touched remoteClient) each loop.
func TestAgentServer_RestoreRace_NoDataRace(t *testing.T) {
	ep := AgentServerEndpoint{URL: "wss://127.0.0.1:9", Token: "tok", Fingerprint: validFingerprint}
	newRes := func() ProvisionResult {
		return ProvisionResult{
			Backend:  &dockerBackend{containerID: "c"},
			Endpoint: &ep,
			Teardown: func() error { return nil },
		}
	}

	i := &Instance{Title: "race", backend: &dockerBackend{containerID: "c0"}}
	// Seed a live remote binding so the very first AgentServer() reads a non-nil
	// remoteClient (the racy branch), matching a restored sandbox mid-poll.
	if err := i.bindProvisionResult(newRes()); err != nil {
		t.Fatalf("seed bindProvisionResult: %v", err)
	}

	const iters = 2000
	var wg sync.WaitGroup
	wg.Add(2)

	// Reader: the daemon poll's refreshInstanceStatus -> instance.AgentServer().
	go func() {
		defer wg.Done()
		for n := 0; n < iters; n++ {
			_ = i.AgentServer()
		}
	}()

	// Writer: restore/recover's reprovisionRemote -> bindProvisionResult, rebinding
	// remoteClient/runtimeTeardown and clearing the cache each time.
	go func() {
		defer wg.Done()
		for n := 0; n < iters; n++ {
			if err := i.bindProvisionResult(newRes()); err != nil {
				t.Errorf("bindProvisionResult: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}
