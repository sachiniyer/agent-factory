package session

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentServer_RestoreRace_NoDataRace is the -race regression for #1729: the
// daemon poll calls Instance.AgentServer() (which reads remoteClient/
// runtimeTeardown to pick the impl) concurrently with a restore/recover that
// rebinds those fields via bindProvisionResult. Originally AgentServer() read
// remoteClient under agentSrvMu while bindProvisionResult wrote it under i.mu —
// two unrelated mutexes, a Go-memory-model data race. Both the cache and its
// source fields now live under i.mu, so `go test -race` flags the original code
// and passes clean here.
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

// TestAgentServer_RestoreCacheConsistency is the regression for the stale-cache
// TOCTOU #1729's first fix introduced (and this structural fix removes): with the
// cache (agentSrv) and its source fields (remoteClient/runtimeTeardown) under
// SEPARATE mutexes, AgentServer() could snapshot the OLD remoteClient, then — after
// a restore cleared the cache and swapped in a NEW client — rebuild the cache from
// that stale snapshot, pinning the torn-down endpoint until the next cache clear.
//
// The invariant that must hold once cache + fields share i.mu: whenever the cached
// agentSrv is a remoteAgentServer, it is bound to the CURRENT remoteClient. A
// checker reads both under one i.mu critical section (a consistent snapshot) while
// the poll builds the cache and restore rebinds a FRESH client each round; any
// stale cache violates rc == remoteClient. It also asserts the FINAL cached server
// reflects the last rebind, never a superseded runtime. Runs under -race too.
func TestAgentServer_RestoreCacheConsistency(t *testing.T) {
	ep := AgentServerEndpoint{URL: "wss://127.0.0.1:9", Token: "tok", Fingerprint: validFingerprint}
	newRes := func() ProvisionResult {
		return ProvisionResult{
			Backend:  &dockerBackend{containerID: "c"},
			Endpoint: &ep,
			Teardown: func() error { return nil },
		}
	}

	i := &Instance{Title: "consistency", backend: &dockerBackend{containerID: "c0"}}
	require.NoError(t, i.bindProvisionResult(newRes()))

	const iters = 3000
	var wg sync.WaitGroup
	wg.Add(3)

	// Restore/recover: rebinds a FRESH remote client (a distinct pointer) each round,
	// clearing the cache in the same i.mu section.
	go func() {
		defer wg.Done()
		for n := 0; n < iters; n++ {
			if err := i.bindProvisionResult(newRes()); err != nil {
				t.Errorf("bindProvisionResult: %v", err)
				return
			}
		}
	}()
	// Daemon poll: builds/returns the cached agent-server.
	go func() {
		defer wg.Done()
		for n := 0; n < iters; n++ {
			_ = i.AgentServer()
		}
	}()
	// Invariant checker: the cached agentSrv, when a remoteAgentServer, MUST be bound
	// to the current remoteClient. rc is set once at construction and never mutated,
	// so reading it outside i.mu is safe; agentSrv/remoteClient are read together
	// under i.mu for a consistent snapshot.
	go func() {
		defer wg.Done()
		for n := 0; n < iters; n++ {
			i.mu.RLock()
			as := i.agentSrv
			rc := i.remoteClient
			i.mu.RUnlock()
			if r, ok := as.(*remoteAgentServer); ok && r.rc != rc {
				t.Errorf("stale agent-server cache: bound to %p but current remoteClient is %p", r.rc, rc)
				return
			}
		}
	}()
	wg.Wait()

	// Final rebind to a fresh runtime, then AgentServer() must reflect exactly it.
	require.NoError(t, i.bindProvisionResult(newRes()))
	r, ok := i.AgentServer().(*remoteAgentServer)
	require.True(t, ok, "expected a remote agent-server")
	i.mu.RLock()
	wantRC := i.remoteClient
	i.mu.RUnlock()
	assert.Same(t, wantRC, r.rc, "AgentServer must reflect the final rebound runtime, never a stale one")
}
