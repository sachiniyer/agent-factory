package session

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"strings"
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
	recorded := config.SSHConfig{Host: "box1.example", Port: 0}
	p := PushPlacement{ListedHost: "box1", Destination: recorded, CallbackRef: "r"}
	cfg := func(entry config.HostConfig) *config.Config {
		return &config.Config{Hosts: map[string]config.HostConfig{"box1": entry}}
	}
	for name, tc := range map[string]struct {
		entry config.HostConfig
		want  bool
	}{
		"the same machine":              {config.HostConfig{Host: "box1.example", PushCredentials: true}, true},
		"case-insensitive host":         {config.HostConfig{Host: "BOX1.example", PushCredentials: true}, true},
		"explicit default port":         {config.HostConfig{Host: "box1.example", Port: 22, PushCredentials: true}, true},
		"another identity file":         {config.HostConfig{Host: "box1.example", IdentityFile: "/k", PushCredentials: true}, true},
		"re-pointed to another machine": {config.HostConfig{Host: "box2.example", PushCredentials: true}, false},
		"another login":                 {config.HostConfig{Host: "box1.example", User: "root", PushCredentials: true}, false},
		"another port":                  {config.HostConfig{Host: "box1.example", Port: 2222, PushCredentials: true}, false},
		"opted out":                     {config.HostConfig{Host: "box1.example"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, p.Licensed(cfg(tc.entry), string(BackendSSH)))
		})
	}
	assert.False(t, p.Licensed(&config.Config{}, string(BackendSSH)), "a host no longer listed")
	assert.False(t, p.Licensed(cfg(config.HostConfig{Host: "box1.example", PushCredentials: true}), string(BackendSandbox)),
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

// Codex P1 on #6433 at 3abfd87: a sandbox.ssh's destination can be remapped by
// ssh_config (-F, ~/.ssh/config, HostName), so the machine is what ssh itself
// reports with -G, and anything af can't prove is Unknown.
func TestSandboxSSHMachineIsWhatSSHActuallyConnectsTo(t *testing.T) {
	t.Cleanup(SetMachineLookupForTest(map[string][]string{
		"box1.example": {"198.51.100.7"},
		"box9":         {"198.51.100.9"},
	}))
	var asked [][]string
	t.Cleanup(SetSSHPrintConfigForTest(func(ssh string, args []string) (string, error) {
		asked = append(asked, append([]string{ssh}, args...))
		switch strings.Join(args, " ") {
		case "-F /etc/af/ssh.conf 192.0.2.10": // a config whose HostName remaps it
			return "user dev\nhostname 198.51.100.7\nport 22\n", nil
		case "-p 2222 -i ~/.ssh/k dev@box9":
			return "hostname box9\nport 2222\n", nil
		case "jump-alias":
			return "hostname box9\nproxycommand /usr/local/bin/route-somewhere %h\n", nil
		case "broken":
			return "", errors.New("ssh: Could not resolve hostname")
		}
		return "", errors.New("unexpected ssh -G call")
	}))
	box1 := RuntimeMachine{Host: "box1.example", Addr: "198.51.100.7"}
	free := func(cmd string) RuntimeMachine { return RuntimeMachine{SandboxSSH: cmd} }

	assert.True(t, free("ssh -F /etc/af/ssh.conf 192.0.2.10").Shares(box1), "the command line names 192.0.2.10, but ssh connects to box1")
	assert.Equal(t, []string{"ssh", "-G", "-F", "/etc/af/ssh.conf", "192.0.2.10"}, append([]string{asked[0][0], "-G"}, asked[0][1:]...),
		"ssh is asked with the command's own arguments")
	assert.False(t, free("ssh -p 2222 -i ~/.ssh/k dev@box9").Shares(box1), "anti-vacuous: a proven other machine differs")

	for name, cmd := range map[string]string{
		"a ProxyCommand in effect": "ssh jump-alias",
		"ssh -G fails":             "ssh broken",
		"quoted shell syntax":      `ssh -o "ProxyCommand=nc %h %p" box9`,
		"a shell operator":         "ssh box9; true",
		"a wrapper script":         "/usr/local/bin/to-box",
	} {
		assert.Truef(t, free(cmd).Shares(RuntimeMachine{Host: "box9"}), "%s: unproven, so it shares every machine", name)
		assert.Truef(t, free(cmd).Shares(box1), "%s: even one it names nothing of", name)
	}
}

// Fourth review of #6433, 2: RuntimeMachine copies under the instance lock and
// resolves nothing there, so a slow resolver can never stall a writer.
func TestRuntimeMachineResolvesNothingUnderTheInstanceLock(t *testing.T) {
	var lookups int
	prev := machineLookup
	machineLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		lookups++
		return nil, errors.New("no DNS in this test")
	}
	t.Cleanup(func() { machineLookup = prev; flushMachineLookups() })
	flushMachineLookups()
	for _, inst := range []*Instance{
		{Title: "a", backend: &sshBackend{cleanup: &SSHRuntimeCleanupData{SessionDir: "/s", HostName: "box1", Config: config.SSHConfig{Host: "box1.example"}}}},
		{Title: "b", backend: &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{SessionDir: "/s", SSHCommand: "ssh box3"}}},
	} {
		_, ok := inst.RuntimeMachine()
		require.True(t, ok)
	}
	assert.Zero(t, lookups, "RuntimeMachine resolved a name, which it would do under the instance lock")
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

func TestPlannedRuntimeMachineResolvesTheTargetTheRuntimeWill(t *testing.T) {
	t.Setenv("GH_HOST", "")
	cfg := &config.ResolvedConfig{}
	cfg.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example"}}
	cfg.SandboxSSH = "ssh freebox"
	prev := resolvedConfigFor
	resolvedConfigFor = func(string) (*config.ResolvedConfig, error) { return cfg, nil }
	t.Cleanup(func() { resolvedConfigFor = prev })

	m, offBox, err := PlannedRuntimeMachine(InstanceOptions{Host: "box1"}, t.TempDir())
	require.NoError(t, err)
	require.True(t, offBox)
	assert.Equal(t, RuntimeMachine{Host: "box1.example"}, m, "a named listed host is the operator's placement")

	cfg.DefaultHost = "box1"
	m, _, _ = PlannedRuntimeMachine(InstanceOptions{Backend: BackendSSH}, t.TempDir())
	assert.Equal(t, RuntimeMachine{Host: "box1.example"}, m, "default_host too")

	cfg.DefaultHost = ""
	cfg.SSH = &config.SSHConfig{Host: "repo-box.example"}
	m, _, _ = PlannedRuntimeMachine(InstanceOptions{Backend: BackendSSH}, t.TempDir())
	assert.Equal(t, RuntimeMachine{Host: "repo-box.example", RepoChosen: true}, m, "a repository's own [ssh] target")

	_, _, err = PlannedRuntimeMachine(InstanceOptions{Host: "not-listed"}, t.TempDir())
	assert.Error(t, err, "a target the runtime can't resolve provisions nothing")

	m, _, _ = PlannedRuntimeMachine(InstanceOptions{Backend: BackendSandbox}, t.TempDir())
	assert.Equal(t, RuntimeMachine{SandboxSSH: "ssh freebox"}, m)

	_, offBox, _ = PlannedRuntimeMachine(InstanceOptions{Backend: BackendLocal}, t.TempDir())
	assert.False(t, offBox, "a local session gets no callback")
}

// recorder is a fake PlacementRecorder, attached as the instance's minter.
type recorder struct {
	fakeCreds
	placed []Placement
	err    error
}

func (r *recorder) RecordPlacement(p Placement) error {
	if r.err != nil {
		return r.err
	}
	r.placed = append(r.placed, p)
	return nil
}

// Design steer on #6433: a create the operator didn't place records its machine
// BEFORE the backend runs anything there, and the record outlives a failed
// create; an operator-placed create records nothing; a record that can't be
// written provisions nothing.
func TestProvisionRecordsThePlacementBeforeTheBackendRuns(t *testing.T) {
	plan := RuntimeMachine{Host: "box1.example", RepoChosen: true}
	t.Cleanup(SetPlannedRuntimeMachineForTest(func(InstanceOptions, string) (RuntimeMachine, bool, error) { return plan, true, nil }))
	rec := &recorder{}
	var seenBeforeBackend int
	t.Cleanup(SetBackendFactoryForTest(func(InstanceOptions, string) (Backend, error) {
		seenBeforeBackend = len(rec.placed)
		return nil, errors.New("startup failed after the hooks ran")
	}))
	_, err := NewInstance(InstanceOptions{Title: "repo", Path: t.TempDir(), Program: "claude", SandboxCredentials: rec})
	require.Error(t, err)
	assert.Equal(t, 1, seenBeforeBackend, "recorded before the backend ran")
	require.Len(t, rec.placed, 1, "and kept when the create fails")
	assert.Equal(t, "repo", rec.placed[0].Title)
	assert.Contains(t, rec.placed[0].Why, "repository's own [ssh] target")

	plan = RuntimeMachine{Host: "box1.example"} // a listed host the operator named
	rec.placed = nil
	_, _ = NewInstance(InstanceOptions{Title: "op", Path: t.TempDir(), Program: "claude", SandboxCredentials: rec})
	assert.Empty(t, rec.placed, "the operator's own placement taints nothing")

	_, _ = NewInstance(InstanceOptions{Title: "child", Path: t.TempDir(), Program: "claude", SandboxCredentials: rec, CreatedBySandbox: "parent"})
	require.Len(t, rec.placed, 1, "a sandbox-created create taints wherever it goes")

	rec.err, seenBeforeBackend = errors.New("disk full"), -1
	_, err = NewInstance(InstanceOptions{Title: "child2", Path: t.TempDir(), Program: "claude", SandboxCredentials: rec, CreatedBySandbox: "parent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot record where this session's code will run")
	assert.Equal(t, -1, seenBeforeBackend, "nothing was provisioned")
}

// And a re-provision of such a session records the machine it is going to
// before it provisions; a record that can't be written refuses it (Codex P1 on
// #6433: every machine a replaced runtime occupies stays tainted).
func TestReprovisionRecordsThePlacementFirst(t *testing.T) {
	t.Cleanup(SetPlannedRuntimeMachineForTest(func(InstanceOptions, string) (RuntimeMachine, bool, error) {
		return RuntimeMachine{Host: "box2.example"}, true, nil
	}))
	rec := &recorder{err: errors.New("disk full")}
	inst := &Instance{Title: "child", Path: t.TempDir(), Host: "box2", sandboxCreds: rec,
		provenance: provenance{createdBySandbox: "parent"},
		backend:    &sshBackend{cleanup: &SSHRuntimeCleanupData{SessionDir: "/s", HostName: "box1", Config: config.SSHConfig{Host: "box1.example"}}}}
	err := inst.reprovisionRemote()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot record where its re-provisioned code will run")

	rec.err = nil
	require.NoError(t, inst.recordReprovisionPlacement())
	require.Len(t, rec.placed, 1)
	assert.Equal(t, "box2.example", rec.placed[0].Machine.Host, "the machine it is going to")

	rec.placed = nil
	inst.provenance = provenance{createdByOperator: true}
	require.NoError(t, inst.recordReprovisionPlacement())
	assert.Empty(t, rec.placed, "an operator session on a host the operator named taints nothing")
}

// Fourth taint review of #6433, 1: a placement taints the whole MACHINE,
// whichever login it ran as. af can't prove a login is unprivileged, and a
// sudo-capable one (a cloud VM's default ubuntu) reads every other login's files.
func TestAPlacementTaintsTheWholeMachineWhateverItsLogin(t *testing.T) {
	t.Cleanup(SetMachineLookupForTest(map[string][]string{"box1.example": {"198.51.100.7"}}))
	pushUser := HostMachine(config.HostConfig{Host: "box1.example", User: "af-push"}).Identity()
	sudoer := HostMachine(config.HostConfig{Host: "box1.example", User: "ubuntu"}).Identity()
	assert.True(t, sudoer.Shares(pushUser), "a placement as ubuntu taints af-push's machine")
	assert.Equal(t, pushUser, sudoer, "the login is no part of the machine's identity")
}

// Fourth taint review of #6433, 3: a placement that must be recorded and can't
// be is refused, not provisioned unrecorded: a plan the runtime can't resolve,
// and no recorder to take it.
func TestAPlacementThatCantBeRecordedIsNotProvisioned(t *testing.T) {
	provisioned := false
	t.Cleanup(SetBackendFactoryForTest(func(InstanceOptions, string) (Backend, error) {
		provisioned = true
		return nil, errors.New("not reached")
	}))
	restore := SetPlannedRuntimeMachineForTest(func(InstanceOptions, string) (RuntimeMachine, bool, error) {
		return RuntimeMachine{}, true, errors.New(`backend=ssh: host "typo" is not in the host list`)
	})
	_, err := NewInstance(InstanceOptions{Title: "child", Path: t.TempDir(), Program: "claude", SandboxCredentials: &recorder{}, CreatedBySandbox: "p"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the host list", "the runtime's own error")
	assert.False(t, provisioned)
	restore()

	t.Cleanup(SetPlannedRuntimeMachineForTest(func(InstanceOptions, string) (RuntimeMachine, bool, error) {
		return RuntimeMachine{Host: "box1.example"}, true, nil
	}))
	_, err = NewInstance(InstanceOptions{Title: "child", Path: t.TempDir(), Program: "claude", SandboxCredentials: &fakeCreds{}, CreatedBySandbox: "p"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no daemon is attached to record where it will run")
	assert.False(t, provisioned, "no recorder: nothing provisioned")
}
