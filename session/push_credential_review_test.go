package session

import (
	"os/exec"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The session half of the independent security review of #6433 (W7.8).

// --- finding 5: the licence follows the machine, not the host's name ----------

func TestPushPlacementLicenceComparesTheRecordedDestination(t *testing.T) {
	recorded := config.SSHConfig{Host: "box1.example", User: "dev", Port: 0}
	p := PushPlacement{ListedHost: "box1", Destination: recorded, CallbackRef: "r"}
	cfg := func(entry config.HostConfig) *config.Config {
		return &config.Config{Hosts: map[string]config.HostConfig{"box1": entry}}
	}
	for name, tc := range map[string]struct {
		entry config.HostConfig
		want  bool
	}{
		"the same machine":              {config.HostConfig{Host: "box1.example", User: "dev", PushCredentials: true}, true},
		"case-insensitive host":         {config.HostConfig{Host: "BOX1.example", User: "dev", PushCredentials: true}, true},
		"explicit default port":         {config.HostConfig{Host: "box1.example", User: "dev", Port: 22, PushCredentials: true}, true},
		"another identity file":         {config.HostConfig{Host: "box1.example", User: "dev", IdentityFile: "/k", PushCredentials: true}, true},
		"re-pointed to another machine": {config.HostConfig{Host: "box2.example", User: "dev", PushCredentials: true}, false},
		"another login":                 {config.HostConfig{Host: "box1.example", User: "root", PushCredentials: true}, false},
		"another port":                  {config.HostConfig{Host: "box1.example", User: "dev", Port: 2222, PushCredentials: true}, false},
		"opted out":                     {config.HostConfig{Host: "box1.example", User: "dev"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, p.Licensed(cfg(tc.entry), string(BackendSSH)))
		})
	}
	assert.False(t, p.Licensed(&config.Config{}, string(BackendSSH)), "a host no longer listed")
	assert.False(t, p.Licensed(cfg(config.HostConfig{Host: "box1.example", User: "dev", PushCredentials: true}), string(BackendSandbox)),
		"a listed placement on a sandbox row")

	free := PushPlacement{FreeFormSandbox: true, SandboxSSH: "ssh box3", CallbackRef: "r"}
	assert.True(t, free.Licensed(&config.Config{SandboxSSH: " ssh box3 ", SandboxPushCredentials: true}, string(BackendSandbox)))
	assert.False(t, free.Licensed(&config.Config{SandboxSSH: "ssh box4", SandboxPushCredentials: true}, string(BackendSandbox)),
		"a changed sandbox.ssh reaches another target")
	assert.False(t, free.Licensed(&config.Config{SandboxSSH: "ssh box3"}, string(BackendSandbox)))
	assert.False(t, (PushPlacement{FreeFormSandbox: true}).Licensed(&config.Config{SandboxPushCredentials: true}, string(BackendSandbox)),
		"no recorded command matches nothing")
}

// --- finding 6: the placement is read under the instance lock -----------------

// Run with -race (CI does): a teardown clears the runtime handle under the
// instance lock while the daemon reads the placement.
func TestPushCredentialPlacementIsReadUnderTheInstanceLock(t *testing.T) {
	b := &sshBackend{cleanup: &SSHRuntimeCleanupData{SessionDir: "/s", HostName: "box1", CallbackRef: "r"}}
	inst := &Instance{Title: "t", backend: b}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_, _ = inst.PushCredentialPlacement()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			inst.mu.Lock()
			if i%2 == 0 {
				b.clearRuntimeCleanupData()
			} else {
				b.cleanup = &SSHRuntimeCleanupData{SessionDir: "/s", HostName: "box1", CallbackRef: "r"}
			}
			inst.mu.Unlock()
		}
	}()
	wg.Wait()
}

// --- finding 1: provenance is durable -------------------------------------------

func TestSandboxCreatedProvenanceSurvivesStorage(t *testing.T) {
	data := InstanceData{
		ID: "child-1", Title: "child", Path: t.TempDir(), Status: Archived, BackendType: "local",
		Worktree:         GitWorktreeData{RepoPath: "/r", WorktreePath: "/r/wt"},
		CreatedBySandbox: "parent-9",
	}
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	assert.Equal(t, "parent-9", restored.CreatedBySandbox())
	assert.Equal(t, "parent-9", restored.ToInstanceData().CreatedBySandbox, "and is written back on the next save")

	operator := &Instance{ID: "op-1", Title: "op"}
	assert.Empty(t, operator.ToInstanceData().CreatedBySandbox)
}

// --- finding 2: the carrier is the session's own tunnel process group ---------

// startCarrierTunnel publishes a live tunnel generation for p: a real child in
// its own process group, as spawnTunnel starts one.
func startCarrierTunnel(t *testing.T, p *sandboxProvisioner) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	child := newTunnelChild(cmd, nil)
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL) // our own recorded child
		<-child.done
	})
	p.reapMu.Lock()
	p.tunnel = child
	p.reapMu.Unlock()
	registerSandboxTunnel(p)
	t.Cleanup(func() { unregisterSandboxTunnel(p) })
	return pid
}

func TestCallbackCarrierRefNamesOnlyTheOwnTunnel(t *testing.T) {
	a := &sandboxProvisioner{reverseFwds: []string{"/h/a.sock:/d.sock"}, callbackRef: "ref-a"}
	b := &sandboxProvisioner{reverseFwds: []string{"/h/b.sock:/d.sock"}, callbackRef: "ref-b"}
	pgidA, pgidB := startCarrierTunnel(t, a), startCarrierTunnel(t, b)

	ref, ok := CallbackCarrierRef(pgidA)
	require.True(t, ok)
	assert.Equal(t, "ref-a", ref)
	ref, ok = CallbackCarrierRef(pgidB)
	require.True(t, ok)
	assert.Equal(t, "ref-b", ref, "each forward names its own session's callback identity")
	_, ok = CallbackCarrierRef(pgidA + pgidB)
	assert.False(t, ok, "an unknown group carries nothing")

	// A provisioner whose tunnel lock is held by a reap carries nothing (refusing
	// is the safe side), and one that has stopped carries nothing.
	a.reapMu.Lock()
	_, ok = CallbackCarrierRef(pgidA)
	a.reapMu.Unlock()
	assert.False(t, ok)
	a.reapMu.Lock()
	a.stopped = true
	a.reapMu.Unlock()
	_, ok = CallbackCarrierRef(pgidA)
	assert.False(t, ok)

	// A tunnel with no callback forward carries no callback.
	b.reapMu.Lock()
	b.reverseFwds = nil
	b.reapMu.Unlock()
	_, ok = CallbackCarrierRef(pgidB)
	assert.False(t, ok)
}
