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

// --- second review, finding 1 and N1 --------------------------------------------

func TestOperatorProvenanceIsPositiveAndUnknownFailsClosed(t *testing.T) {
	op, err := NewInstance(InstanceOptions{Title: "mine", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	assert.True(t, op.OperatorCreated(), "a create no sandbox asked for is the operator's")
	assert.True(t, op.ToInstanceData().CreatedByOperator, "and is recorded on the row")

	child, err := NewInstance(InstanceOptions{Title: "child", Path: t.TempDir(), Program: "claude", CreatedBySandbox: "parent-1"})
	require.NoError(t, err)
	assert.False(t, child.OperatorCreated())
	assert.False(t, child.ToInstanceData().CreatedByOperator)

	base := InstanceData{ID: "r", Title: "r", Path: t.TempDir(), Status: Archived, BackendType: "local",
		Worktree: GitWorktreeData{RepoPath: "/r", WorktreePath: "/r/wt"}}
	for name, tc := range map[string]struct {
		byOperator bool
		bySandbox  string
		want       bool
	}{
		"operator":                       {byOperator: true, want: true},
		"unknown (no provenance at all)": {},
		"sandbox":                        {bySandbox: "p"},
		"both (contradictory)":           {byOperator: true, bySandbox: "p"},
	} {
		t.Run(name, func(t *testing.T) {
			data := base
			data.CreatedByOperator, data.CreatedBySandbox = tc.byOperator, tc.bySandbox
			restored, err := FromInstanceData(data)
			require.NoError(t, err)
			assert.Equal(t, tc.want, restored.OperatorCreated())
			assert.Equal(t, tc.byOperator, restored.ToInstanceData().CreatedByOperator, "round-trips")
		})
	}
}

func TestRuntimeMachineSharesIsConservative(t *testing.T) {
	t.Cleanup(SetMachineLookupForTest(map[string][]string{
		"box1.example": {"198.51.100.7", "2001:db8::7"},
		"box2.example": {"198.51.100.8"},
		"box9":         {"198.51.100.9"},
		"box3":         {"198.51.100.3"},
		"box4":         {"198.51.100.4"},
		// Spellings that resolve ELSEWHERE, so only the naming rules can match
		// them, not a shared address or the fail-closed unknown.
		"box1.example.": {"203.0.113.50"},
		"box1":          {"203.0.113.51"},
	}))
	box1 := RuntimeMachine{Host: "box1.example", Addr: "198.51.100.7"}
	for name, tc := range map[string]struct {
		a, b RuntimeMachine
		want bool
	}{
		"same host":                       {box1, RuntimeMachine{Host: "BOX1.example"}, true},
		"trailing dot":                    {box1, RuntimeMachine{Host: "box1.example."}, true},
		"short name of an FQDN":           {box1, RuntimeMachine{Host: "box1"}, true},
		"same pinned address":             {box1, RuntimeMachine{Host: "box2.example", Addr: "198.51.100.7"}, true},
		"host is the other's address":     {box1, RuntimeMachine{Host: "198.51.100.7"}, true},
		"IPv4-mapped IPv6":                {box1, RuntimeMachine{Host: "::ffff:198.51.100.7"}, true},
		"another IPv6 spelling":           {box1, RuntimeMachine{Host: "2001:0db8:0:0::7"}, true},
		"two addresses of one name":       {RuntimeMachine{Host: "2001:db8::7"}, RuntimeMachine{Host: "198.51.100.7", Addr: "198.51.100.7"}, false},
		"the name links both addresses":   {RuntimeMachine{Host: "2001:db8::7"}, box1, true},
		"another machine":                 {box1, RuntimeMachine{Host: "box2.example", Addr: "198.51.100.8"}, false},
		"an unresolvable name":            {box1, RuntimeMachine{Host: "nowhere.example"}, true},
		"an in-flight provision":          {box1, RuntimeMachine{Unknown: true}, true},
		"same free-form destination":      {RuntimeMachine{SandboxSSH: "ssh box3"}, RuntimeMachine{SandboxSSH: " ssh  box3 "}, true},
		"other free-form destination":     {RuntimeMachine{SandboxSSH: "ssh box3"}, RuntimeMachine{SandboxSSH: "ssh box4"}, false},
		"free-form naming the ssh host":   {RuntimeMachine{SandboxSSH: "ssh -p 2222 -i /k dev@box1.example"}, box1, true},
		"free-form url naming an address": {box1, RuntimeMachine{SandboxSSH: "ssh ssh://dev@198.51.100.7:22"}, true},
		"free-form HostName override":     {box1, RuntimeMachine{SandboxSSH: "ssh -o HostName=198.51.100.7 alias"}, true},
		"free-form naming another":        {RuntimeMachine{SandboxSSH: "ssh dev@box9"}, box1, false},
		"free-form ssh_config alias":      {RuntimeMachine{SandboxSSH: "ssh myalias"}, box1, true},
		"free-form wrapper script":        {RuntimeMachine{SandboxSSH: "/usr/local/bin/to-box"}, box1, true},
		"nothing recorded":                {RuntimeMachine{}, box1, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.a.Shares(tc.b))
			assert.Equal(t, tc.want, tc.b.Shares(tc.a), "symmetric")
		})
	}
}

func TestRuntimeMachineReadsTheRecordedRuntime(t *testing.T) {
	listed := &Instance{Title: "l", backend: &sshBackend{cleanup: &SSHRuntimeCleanupData{SessionDir: "/s", HostName: "box1",
		Config: config.SSHConfig{Host: "box1.example"}, DialAddress: "198.51.100.7"}}}
	m, ok := listed.RuntimeMachine()
	require.True(t, ok)
	assert.Equal(t, RuntimeMachine{Host: "box1.example", Addr: "198.51.100.7"}, m)
	p, ok := listed.PushCredentialPlacement()
	require.True(t, ok)
	assert.Equal(t, m, p.Machine, "the placement carries the same machine")

	repoTarget := &Instance{Title: "r", backend: &sshBackend{cleanup: &SSHRuntimeCleanupData{SessionDir: "/s", Config: config.SSHConfig{Host: "box1.example"}}}}
	m, ok = repoTarget.RuntimeMachine()
	require.True(t, ok, "a repository's own [ssh] target still runs on a machine")
	assert.Equal(t, RuntimeMachine{Host: "box1.example", RepoChosen: true}, m, "one the repository chose")

	free := &Instance{Title: "f", backend: &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{SessionDir: "/s", SSHCommand: "ssh box3"}}}
	m, ok = free.RuntimeMachine()
	require.True(t, ok)
	assert.Equal(t, RuntimeMachine{SandboxSSH: "ssh box3"}, m)

	for name, inst := range map[string]*Instance{
		"reaped": {Title: "x", backend: &sshBackend{}},
		"local":  {Title: "y", backend: &LocalBackend{}},
	} {
		_, ok := inst.RuntimeMachine()
		assert.False(t, ok, name)
	}
}

// Codex P2 on #6433 at 50552b4: an archive af confirmed stopped while keeping its
// workspace runs nothing, so it blocks no host — but only while every condition
// of "confirmed stopped" holds.
func TestStoppedPreservedArchiveNeedsEveryConditionOfAConfirmedStop(t *testing.T) {
	stopped := func() *Instance {
		return &Instance{Title: "a", liveness: LiveArchived, archivedWorkspace: &ArchivedWorkspaceData{Host: "box1", Dir: "/s"}}
	}
	assert.True(t, stopped().StoppedPreservedArchive())
	for name, mutate := range map[string]func(*Instance){
		"no preserved workspace":        func(i *Instance) { i.archivedWorkspace = nil },
		"not archived (being restored)": func(i *Instance) { i.liveness = LiveReady },
		"started":                       func(i *Instance) { i.started = true },
		"an agent server attached":      func(i *Instance) { i.agentSrv = &remoteAgentServer{} },
		"stop not confirmed":            func(i *Instance) { i.runtimeCleanupStateUnknown = true },
	} {
		i := stopped()
		mutate(i)
		assert.False(t, i.StoppedPreservedArchive(), name)
	}

	// And it survives storage: a restarted daemon still knows the archive stopped.
	data := InstanceData{ID: "a1", Title: "a", Path: t.TempDir(), Status: Archived, BackendType: "ssh", CreatedBySandbox: "p",
		Worktree:          GitWorktreeData{RepoPath: "/r", WorktreePath: "/r/wt"},
		ArchivedWorkspace: &ArchivedWorkspaceData{Host: "box1", Dir: "/s"}}
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	assert.True(t, restored.StoppedPreservedArchive())
}

// Third review of #6433, B: while a re-provision replaces the runtime, the
// machine is Unknown (fail closed), and an archive mid-restore is not "stopped".
func TestRuntimeMachineIsUnknownWhileAReprovisionRuns(t *testing.T) {
	listed := &Instance{Title: "l", Host: "box1", backend: &sshBackend{cleanup: &SSHRuntimeCleanupData{SessionDir: "/s", HostName: "box1",
		Config: config.SSHConfig{Host: "box1.example"}}}}
	end := listed.beginProvisioning()
	m, ok := listed.RuntimeMachine()
	require.True(t, ok)
	assert.Equal(t, RuntimeMachine{Unknown: true}, m, "a listed host the operator named stays operator-placed")
	end()
	end() // idempotent
	m, _ = listed.RuntimeMachine()
	assert.False(t, m.Unknown)

	reaped := &Instance{Title: "r", backend: &sshBackend{}}
	_, ok = reaped.RuntimeMachine()
	assert.False(t, ok, "a reaped runtime runs nowhere")
	end = reaped.beginProvisioning()
	m, ok = reaped.RuntimeMachine()
	require.True(t, ok, "but one being re-provisioned runs somewhere af can't name")
	assert.Equal(t, RuntimeMachine{Unknown: true, RepoChosen: true}, m, "with no named host, not shown to be the operator's placement")
	end()

	archived := &Instance{Title: "a", liveness: LiveArchived, archivedWorkspace: &ArchivedWorkspaceData{Host: "box1", Dir: "/s"}}
	end = archived.beginProvisioning()
	assert.False(t, archived.StoppedPreservedArchive(), "an archive being restored is not stopped")
	end()
	assert.True(t, archived.StoppedPreservedArchive())
}

func TestPlannedRuntimeMachineResolvesTheTargetTheRuntimeWill(t *testing.T) {
	t.Setenv("GH_HOST", "")
	cfg := &config.ResolvedConfig{}
	cfg.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example"}}
	cfg.SandboxSSH = "ssh freebox"
	prev := resolvedConfigFor
	resolvedConfigFor = func(string) (*config.ResolvedConfig, error) { return cfg, nil }
	t.Cleanup(func() { resolvedConfigFor = prev })

	m, offBox := PlannedRuntimeMachine(InstanceOptions{Host: "box1"}, t.TempDir())
	require.True(t, offBox)
	assert.Equal(t, RuntimeMachine{Host: "box1.example"}, m, "a named listed host is the operator's placement")

	cfg.DefaultHost = "box1"
	m, _ = PlannedRuntimeMachine(InstanceOptions{Backend: BackendSSH}, t.TempDir())
	assert.Equal(t, RuntimeMachine{Host: "box1.example"}, m, "default_host too")

	cfg.DefaultHost = ""
	cfg.SSH = &config.SSHConfig{Host: "repo-box.example"}
	m, _ = PlannedRuntimeMachine(InstanceOptions{Backend: BackendSSH}, t.TempDir())
	assert.Equal(t, RuntimeMachine{Host: "repo-box.example", RepoChosen: true}, m, "a repository's own [ssh] target")

	m, _ = PlannedRuntimeMachine(InstanceOptions{Host: "not-listed"}, t.TempDir())
	assert.True(t, m.Unknown, "a target that can't be resolved is Unknown")

	m, _ = PlannedRuntimeMachine(InstanceOptions{Backend: BackendSandbox}, t.TempDir())
	assert.Equal(t, RuntimeMachine{SandboxSSH: "ssh freebox"}, m)

	_, offBox = PlannedRuntimeMachine(InstanceOptions{Backend: BackendLocal}, t.TempDir())
	assert.False(t, offBox, "a local session gets no callback")
}
