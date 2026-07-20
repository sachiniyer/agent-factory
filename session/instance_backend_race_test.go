package session

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCapabilitiesRacesRestoreBackendSwap reproduces #2096: Capabilities() read
// i.backend bare while a concurrent restore swapped it under i.mu in
// bindProvisionResult. The restore paths (daemon/restore.go,
// daemon/lostrestore.go) consult Capabilities().Recover BEFORE taking the
// per-instance opLock, so the read genuinely overlaps another goroutine's
// re-provision of the same instance. Under -race this fails on the pre-fix code
// with a DATA RACE on Instance.backend.
func TestCapabilitiesRacesRestoreBackendSwap(t *testing.T) {
	i := &Instance{Title: "s", backend: newInertSandboxBackend("docker")}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			// Endpoint nil keeps this off the network: bindProvisionResult builds
			// no remote client and just swaps the runtime fields, which is the
			// write half of the race.
			assert.NoError(t, i.bindProvisionResult(ProvisionResult{Backend: newInertSandboxBackend("docker")}))
		}
	}()
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			_ = i.Capabilities()
		}
	}()
	wg.Wait()
}

// TestLifecycleViewWithConcurrentBackendSwap pins the reentrancy constraint that
// makes the naive fix wrong: LifecycleView() reads the recover capability while
// already holding i.mu.RLock, so Capabilities() must not take i.mu itself on that
// path. sync.RWMutex is not reentrant — a recursive RLock deadlocks as soon as a
// writer (here, the restoring goroutine's bindProvisionResult) queues between the
// two acquisitions. This test hangs, not races, if that regresses.
func TestLifecycleViewWithConcurrentBackendSwap(t *testing.T) {
	i := &Instance{Title: "s", backend: newInertSandboxBackend("docker")}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			assert.NoError(t, i.bindProvisionResult(ProvisionResult{Backend: newInertSandboxBackend("docker")}))
		}
	}()
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			_ = i.LifecycleView()
		}
	}()
	wg.Wait()
}

// TestCapabilitiesBackendSnapshot is the behavioural lock: synchronizing the read
// must not change what Capabilities() reports. A nil backend still reports local
// full parity, and a bound backend still reports its own descriptor.
func TestCapabilitiesBackendSnapshot(t *testing.T) {
	var unbound Instance
	assert.Equal(t, (&LocalBackend{}).Capabilities(), unbound.Capabilities(),
		"a backend-less instance reports local full parity")

	remote := &Instance{backend: newInertSandboxBackend("docker")}
	assert.Equal(t, WorkspaceRemote, remote.Capabilities().Workspace,
		"a bound backend reports its own capabilities")
}
