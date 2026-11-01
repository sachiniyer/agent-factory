package session

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// Which machine a runtime runs on (#5571 W7.8, security reviews of #6433). Code
// the operator did not place TAINTS the machine it is placed on, durably, before
// any of it runs (RecordPlacement); a tainted machine gets no push credential
// until the operator resets its trust. The whole MACHINE, whichever user the
// code ran as: af can't prove a login is unprivileged, and a sudo-capable one (a
// cloud VM's default user) reads every other user's files. The identity is
// compared CONSERVATIVELY: where two machines cannot be shown to differ, they
// are one.

// RuntimeMachine names the machine an off-box runtime runs on,
// from the handle its provision recorded (or the target a provision will
// resolve): the ssh destination and the address af actually dialed (the ssh
// runtime pins it, ssh_machine_pin.go), or the free-form sandbox.ssh command.
type RuntimeMachine struct {
	// Host is the ssh destination's host, as configured.
	Host string
	// Addr is the address the ssh runtime dialed and pinned; empty when none was
	// recorded.
	Addr string
	// SandboxSSH is the free-form target's sandbox.ssh command.
	SandboxSSH string
	// RepoChosen marks an ssh runtime on a repository's own [ssh] target: a
	// placement the repository chose, not the operator.
	RepoChosen bool
}

// MachineIdentity is a machine's canonical identities: lowercased names without
// a trailing dot, and unmapped, zone-less addresses (as configured, pinned, or
// resolved). Unproven, when set, says why af cannot establish the machine, which
// then counts as every machine. Its Names and Addrs are what a taint persists.
type MachineIdentity struct {
	Names    []string `json:"names,omitempty"`
	Addrs    []string `json:"addrs,omitempty"`
	Unproven string   `json:"unproven,omitempty"`
}

// Empty reports whether the identity names nothing (and so matches nothing).
func (id MachineIdentity) Empty() bool {
	return id.Unproven == "" && len(id.Names) == 0 && len(id.Addrs) == 0
}

// Shares reports whether two identities may be one machine: either is Unproven,
// a canonical name matches (a short name matching a longer one's first label),
// or an address does. It resolves nothing.
func (id MachineIdentity) Shares(o MachineIdentity) bool {
	if id.Empty() || o.Empty() {
		return false
	}
	if id.Unproven != "" || o.Unproven != "" {
		return true
	}
	for _, x := range id.Names {
		for _, y := range o.Names {
			if x == y || shortNameOf(x, y) || shortNameOf(y, x) {
				return true
			}
		}
	}
	for _, a := range id.Addrs {
		for _, b := range o.Addrs {
			if a == b {
				return true
			}
		}
	}
	return false
}

// Merge is the union of two identities of one placement.
func (id MachineIdentity) Merge(o MachineIdentity) MachineIdentity {
	out := MachineIdentity{Names: union(id.Names, o.Names), Addrs: union(id.Addrs, o.Addrs), Unproven: id.Unproven}
	if out.Unproven == "" {
		out.Unproven = o.Unproven
	}
	return out
}

func union(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range append(append([]string(nil), a...), b...) {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// Shares reports whether two runtimes may run on the same machine
// (MachineIdentity.Shares). It may resolve names: never call it under a lock.
func (m RuntimeMachine) Shares(o RuntimeMachine) bool {
	return m.Identity().Shares(o.Identity())
}

// Unproven says why af cannot establish which machine m is ("" when it can, or m
// names nothing). It may resolve names: never call it under a lock.
func (m RuntimeMachine) Unproven() string {
	return m.Identity().Unproven
}

// recorded reports whether m names anything, without resolving it.
func (m RuntimeMachine) recorded() bool {
	return strings.TrimSpace(m.Host) != "" || strings.TrimSpace(m.Addr) != "" || strings.TrimSpace(m.SandboxSSH) != ""
}

// Identity is m's canonical identities, resolving its names (cached, bounded) and
// asking ssh where a sandbox.ssh command goes. A name that does not resolve, or a
// sandbox.ssh whose destination ssh cannot confirm, makes it Unproven.
//
// Not detected: two literal addresses of one multi-homed machine that no name
// links, and a DNS or /etc/hosts answer that differs from what the session's ssh
// dialed (its pinned address is compared too).
func (m RuntimeMachine) Identity() MachineIdentity {
	var names, addrs []string
	unproven := ""
	hosts := []string{m.Host, m.Addr}
	if strings.TrimSpace(m.SandboxSSH) != "" {
		dest, ok := sandboxSSHEffectiveHost(m.SandboxSSH)
		if !ok {
			return MachineIdentity{Unproven: "ssh can't confirm which machine its sandbox.ssh command reaches"}
		}
		hosts = append(hosts, dest)
	}
	for _, h := range hosts {
		h = canonicalMachineName(h)
		if h == "" {
			continue
		}
		if addr, err := netip.ParseAddr(h); err == nil {
			addrs = append(addrs, addr.Unmap().WithZone("").String())
			continue
		}
		names = append(names, h)
		resolved, err := resolveMachine(h)
		if err != nil || len(resolved) == 0 {
			if unproven == "" {
				unproven = "its host name " + h + " doesn't resolve"
			}
			continue
		}
		for _, a := range resolved {
			addrs = append(addrs, a.String())
		}
	}
	return MachineIdentity{Names: union(names, nil), Addrs: union(addrs, nil), Unproven: unproven}
}

// canonicalMachineName lowercases a host, and drops IPv6 brackets and a
// trailing dot.
func canonicalMachineName(h string) string {
	h = strings.ToLower(strings.Trim(strings.TrimSpace(h), "[]"))
	return strings.TrimSuffix(h, ".")
}

// shortNameOf reports whether short, a name with no dot, is the first label of
// long: "box1" and "box1.corp.example" may be one machine.
func shortNameOf(short, long string) bool {
	if strings.Contains(short, ".") {
		return false
	}
	first, _, ok := strings.Cut(long, ".")
	return ok && first == short
}

// sshFlagsWithArgument are the ssh options that take a value (OpenSSH 9).
const sshFlagsWithArgument = "BbcDEeFIiJLlmOoPpQRSWw"

// sandboxSSHEffectiveHost is the machine a sandbox.ssh command actually
// connects to: the hostname ssh itself reports for it with -G, which evaluates
// the command's -F file, ~/.ssh/config and every Host and Match block, and
// connects to nothing. The command line alone proves nothing, because any of
// those can remap its destination with HostName (Codex P1 on #6433). ok is false,
// and the machine Unknown, whenever that is not proven:
//   - the command is not a plain ssh invocation (a wrapper script);
//   - it uses shell syntax (quotes, expansions, operators), which af's tunnel
//     script would interpret and this check will not;
//   - ssh -G fails or prints no hostname;
//   - a ProxyCommand is in effect, which can reach any machine whatever the
//     hostname says.
func sandboxSSHEffectiveHost(command string) (string, bool) {
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, "'\"`$;&|<>(){}*?[]#!\\\n") {
		return "", false
	}
	words := strings.Fields(command)
	if filepath.Base(words[0]) != "ssh" {
		return "", false
	}
	sshEffectiveHosts.mu.Lock()
	if e, ok := sshEffectiveHosts.m[command]; ok && time.Now().Before(e.until) {
		sshEffectiveHosts.mu.Unlock()
		return e.host, e.ok
	}
	sshEffectiveHosts.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), machineLookupTimeout)
	defer cancel()
	out, err := sshPrintConfig(ctx, words[0], words[1:])
	host, ok := "", err == nil
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch strings.ToLower(k) {
		case "hostname":
			host = strings.TrimSpace(v)
		case "proxycommand":
			if v = strings.TrimSpace(v); v != "" && !strings.EqualFold(v, "none") {
				ok = false
			}
		}
	}
	ok = ok && host != ""
	ttl := machineLookupTTL
	if !ok {
		ttl = machineLookupFailTTL
	}
	sshEffectiveHosts.mu.Lock()
	sshEffectiveHosts.m[command] = sshEffectiveHost{host: host, ok: ok, until: time.Now().Add(ttl)}
	sshEffectiveHosts.mu.Unlock()
	return host, ok
}

type sshEffectiveHost struct {
	host  string
	ok    bool
	until time.Time
}

var sshEffectiveHosts = struct {
	mu sync.Mutex
	m  map[string]sshEffectiveHost
}{m: map[string]sshEffectiveHost{}}

// sshPrintConfig runs `<ssh> -G <args…>`: ssh's own reading of the command, with
// no connection. A package var so tests never run a real ssh.
var sshPrintConfig = func(ctx context.Context, ssh string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, ssh, append([]string{"-G"}, args...)...)
	cmd.Stdin = nil
	out, err := cmd.Output()
	return string(out), err
}

// sandboxSSHDestination is the destination host a sandbox.ssh command names on
// its command line: the first non-option argument of an ssh invocation, or a -o
// HostName= override, without a user@, an ssh:// or a :port. It is what ssh -G
// reports for a command with no ssh_config, so tests stand it in for ssh.
func sandboxSSHDestination(command string) (string, bool) {
	words := strings.Fields(command)
	if len(words) == 0 || filepath.Base(words[0]) != "ssh" {
		return "", false
	}
	dest, override := "", ""
	for i := 1; i < len(words); i++ {
		w := words[i]
		if strings.HasPrefix(w, "-") && len(w) > 1 {
			flag := w[1:2]
			if strings.Contains(sshFlagsWithArgument, flag) {
				value := w[2:]
				if value == "" && i+1 < len(words) {
					i++
					value = words[i]
				}
				if k, v, ok := strings.Cut(value, "="); flag == "o" && ok && strings.EqualFold(k, "HostName") {
					override = v
				}
			}
			continue
		}
		if dest == "" {
			dest = w
		}
	}
	if override != "" {
		dest = override
	}
	dest = strings.TrimPrefix(dest, "ssh://")
	if at := strings.LastIndex(dest, "@"); at >= 0 {
		dest = dest[at+1:]
	}
	if h, _, err := net.SplitHostPort(dest); err == nil {
		dest = h
	}
	return dest, dest != ""
}

// machineLookup resolves a host name; a package var so tests never touch DNS.
var machineLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

const (
	machineLookupTimeout = 2 * time.Second
	machineLookupTTL     = time.Minute
	machineLookupFailTTL = 15 * time.Second
)

type machineLookupEntry struct {
	addrs []netip.Addr
	err   error
	until time.Time
}

var machineLookups = struct {
	mu sync.Mutex
	m  map[string]machineLookupEntry
}{m: map[string]machineLookupEntry{}}

// resolveMachine is machineLookup, bounded in time and cached: the host rule
// asks it on every push-credential request, for every candidate session.
func resolveMachine(host string) ([]netip.Addr, error) {
	now := time.Now()
	machineLookups.mu.Lock()
	if e, ok := machineLookups.m[host]; ok && now.Before(e.until) {
		machineLookups.mu.Unlock()
		return e.addrs, e.err
	}
	machineLookups.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), machineLookupTimeout)
	defer cancel()
	raw, err := machineLookup(ctx, host)
	addrs := make([]netip.Addr, 0, len(raw))
	for _, a := range raw {
		addrs = append(addrs, a.Unmap().WithZone(""))
	}
	ttl := machineLookupTTL
	if err != nil {
		ttl = machineLookupFailTTL
	}
	machineLookups.mu.Lock()
	machineLookups.m[host] = machineLookupEntry{addrs: addrs, err: err, until: now.Add(ttl)}
	machineLookups.mu.Unlock()
	return addrs, err
}

// SetMachineLookupForTest makes host-name resolution answer from names (a name
// that is absent does not resolve), and returns a restore function. For tests
// outside this package that place sessions on named machines.
func SetMachineLookupForTest(names map[string][]string) (restore func()) {
	prev := machineLookup
	machineLookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, s := range names[host] {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, err
			}
			out = append(out, a)
		}
		if len(out) == 0 {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return out, nil
	}
	// And ssh -G as it answers with no ssh_config: the command-line destination.
	prevPrint := sshPrintConfig
	sshPrintConfig = func(_ context.Context, ssh string, args []string) (string, error) {
		dest, ok := sandboxSSHDestination(strings.Join(append([]string{ssh}, args...), " "))
		if !ok {
			return "", &net.DNSError{Err: "no destination", Name: ssh}
		}
		return "hostname " + dest + "\nport 22\n", nil
	}
	flushMachineLookups()
	return func() {
		machineLookup, sshPrintConfig = prev, prevPrint
		flushMachineLookups()
	}
}

// SetSSHPrintConfigForTest makes `ssh -G` answer with print, after
// SetMachineLookupForTest, and returns a restore function.
func SetSSHPrintConfigForTest(print func(ssh string, args []string) (string, error)) (restore func()) {
	prev := sshPrintConfig
	sshPrintConfig = func(_ context.Context, ssh string, args []string) (string, error) { return print(ssh, args) }
	flushMachineLookups()
	return func() {
		sshPrintConfig = prev
		flushMachineLookups()
	}
}

func flushMachineLookups() {
	machineLookups.mu.Lock()
	machineLookups.m = map[string]machineLookupEntry{}
	machineLookups.mu.Unlock()
	sshEffectiveHosts.mu.Lock()
	sshEffectiveHosts.m = map[string]sshEffectiveHost{}
	sshEffectiveHosts.mu.Unlock()
}

// RuntimeMachine reports the machine this session's off-box runtime runs on,
// from its runtime handle: any ssh runtime (a listed host or a
// repository's [ssh] target) or the free-form sandbox target. It copies the
// handle under the instance lock and resolves nothing there, so a slow resolver
// can never stall a writer. ok is false with no runtime handle.
func (i *Instance) RuntimeMachine() (RuntimeMachine, bool) {
	i.mu.RLock()
	var data *RuntimeCleanupData
	if provider, ok := i.backend.(runtimeCleanupProvider); ok {
		data = provider.runtimeCleanupData()
	}
	m := machineOf(data)
	i.mu.RUnlock()
	return m, m.recorded()
}

func machineOf(data *RuntimeCleanupData) RuntimeMachine {
	switch {
	case data == nil:
		return RuntimeMachine{}
	case data.SSH != nil:
		addr, _ := sshPinnedCleanupTarget(data.SSH)
		return RuntimeMachine{Host: data.SSH.Config.Host, Addr: addr, RepoChosen: data.SSH.HostName == ""}
	case data.Sandbox != nil:
		return RuntimeMachine{SandboxSSH: data.Sandbox.SSHCommand}
	}
	return RuntimeMachine{}
}

// Placement is one placement of code the operator did not place, as a recorder
// persists it: the session, why it isn't the operator's, and where it runs.
type Placement struct {
	SessionID, Title, Why string
	Machine               RuntimeMachine
}

// PlacementRecorder is the optional half of SandboxCredentials that records a
// Placement DURABLY. It is called before any of the placement's code can run on
// the machine; an error refuses the provision (#5571 W7.8, design steer on #6433).
type PlacementRecorder interface {
	RecordPlacement(p Placement) error
}

// placementWhy is why a session's placement is not the operator's, or "" when it
// is: the operator created it AND chose where it runs.
func placementWhy(createdBySandbox string, createdByOperator bool, m RuntimeMachine) string {
	switch {
	case createdBySandbox != "":
		return "created by another session's af"
	case !createdByOperator:
		return "of unknown provenance (created before af recorded who creates a session)"
	case m.RepoChosen:
		return "on a repository's own [ssh] target, which the repository chose"
	}
	return ""
}

// recordPlacement records p when it is not the operator's placement. With no
// recorder to take it (no daemon attached), the placement is refused rather
// than made unrecorded: a provision of code the operator didn't place never
// runs without its taint written first.
func recordPlacement(creds SandboxCredentials, p Placement) error {
	if p.Why == "" || !p.Machine.recorded() {
		return nil
	}
	recorder, ok := creds.(PlacementRecorder)
	if !ok {
		return fmt.Errorf("no daemon is attached to record where it will run")
	}
	return recorder.RecordPlacement(p)
}

// provisionRecordingPlacement is NewInstance's provision: a create the operator
// did not place has its planned machine recorded BEFORE the backend factory
// runs anything there, and the machine it actually provisioned (with the
// address it dialed) recorded after, whether or not the create later fails.
func provisionRecordingPlacement(opts InstanceOptions, absPath string, kind BackendKind) (ProvisionResult, error) {
	createdByOperator := opts.CreatedBySandbox == ""
	planned, offBox, err := planRuntimeMachine(opts, absPath)
	if err != nil {
		// The runtime resolves its target exactly so, and would fail the same way;
		// refusing here keeps a placement that couldn't be recorded from running.
		return ProvisionResult{}, err
	}
	if offBox {
		why := placementWhy(opts.CreatedBySandbox, createdByOperator, planned)
		if err := recordPlacement(opts.SandboxCredentials, Placement{SessionID: opts.ID, Title: opts.Title, Why: why, Machine: planned}); err != nil {
			return ProvisionResult{}, fmt.Errorf("cannot record where this session's code will run, so it is not provisioned: %w", err)
		}
	}
	res, err := backendFactory(opts, absPath, kind)
	if provider, ok := res.Backend.(runtimeCleanupProvider); ok {
		m := machineOf(provider.runtimeCleanupData())
		if rerr := recordPlacement(opts.SandboxCredentials, Placement{SessionID: opts.ID, Title: opts.Title,
			Why: placementWhy(opts.CreatedBySandbox, createdByOperator, m), Machine: m}); rerr != nil {
			log.WarningLog.Printf("session %q: could not record the address its runtime dialed: %v", opts.Title, rerr)
		}
	}
	return res, err
}

// recordReprovisionPlacement records, before a re-provision runs anything, the
// machine it will provision onto, when the session is not the operator's to
// place. A target the runtime cannot resolve provisions nothing.
func (i *Instance) recordReprovisionPlacement() error {
	i.mu.RLock()
	creds, host, path := i.sandboxCreds, i.Host, i.Path
	var kind BackendKind
	switch i.backend.(type) {
	case *sshBackend:
		kind = BackendSSH
	case *sandboxBackend:
		kind = BackendSandbox
	}
	bySandbox, byOperator, id, title := i.createdBySandbox, i.createdByOperator, i.ID, i.Title
	i.mu.RUnlock()
	if kind == "" {
		return nil
	}
	planned, offBox, err := planRuntimeMachine(InstanceOptions{Host: host, Backend: kind}, path)
	if err != nil {
		return fmt.Errorf("session %q: cannot resolve where it would be re-provisioned: %w", title, err)
	}
	if !offBox {
		return nil
	}
	if err := recordPlacement(creds, Placement{SessionID: id, Title: title, Why: placementWhy(bySandbox, byOperator, planned), Machine: planned}); err != nil {
		return fmt.Errorf("session %q: cannot record where its re-provisioned code will run, so it is not re-provisioned: %w", title, err)
	}
	return nil
}

// recordBoundPlacement records the machine a freshly bound runtime handle names
// (the address it dialed), for a session not the operator's to place.
func (i *Instance) recordBoundPlacement() {
	m, ok := i.RuntimeMachine()
	if !ok {
		return
	}
	i.mu.RLock()
	creds, why, id, title := i.sandboxCreds, placementWhy(i.createdBySandbox, i.createdByOperator, m), i.ID, i.Title
	i.mu.RUnlock()
	if err := recordPlacement(creds, Placement{SessionID: id, Title: title, Why: why, Machine: m}); err != nil {
		log.WarningLog.Printf("session %q: could not record the address its runtime dialed: %v", title, err)
	}
}

// PlacementWhy is why this row's placement is not the operator's ("" when it is),
// for the daemon's taint of rows it restores.
func (i *Instance) PlacementWhy(m RuntimeMachine) string {
	return placementWhy(i.createdBySandbox, i.createdByOperator, m)
}

// PlannedRuntimeMachine is the machine a create with opts would provision onto,
// resolved before it provisions, the way the runtime itself will resolve it:
// the listed host chooseListedHost picks (named, or default_host), the
// repository's own [ssh] target, or the free-form sandbox.ssh target. offBox is
// false for a runtime that gets no af callback (local, docker, hook). An error is
// one the runtime's own resolution hits too, so nothing is provisioned.
func PlannedRuntimeMachine(opts InstanceOptions, repoRoot string) (m RuntimeMachine, offBox bool, err error) {
	kind, err := BackendKindFor(opts, repoRoot)
	if err != nil {
		return RuntimeMachine{}, false, err
	}
	if !kind.InjectsSandboxCallback() {
		return RuntimeMachine{}, false, nil
	}
	cfg, err := resolvedConfigFor(repoRoot)
	if err != nil || cfg == nil {
		return RuntimeMachine{}, true, fmt.Errorf("cannot resolve the config: %w", err)
	}
	if kind == BackendSandbox {
		return RuntimeMachine{SandboxSSH: cfg.SandboxSSH}, true, nil
	}
	name, listed, err := chooseListedHost(opts.Host, cfg)
	switch {
	case err != nil:
		return RuntimeMachine{}, true, err
	case name != "" && listed != nil:
		return RuntimeMachine{Host: listed.Host}, true, nil
	case cfg.SSH != nil && strings.TrimSpace(cfg.SSH.Host) != "":
		return RuntimeMachine{Host: cfg.SSH.Host, RepoChosen: true}, true, nil
	}
	return RuntimeMachine{}, true, fmt.Errorf("no ssh target is configured")
}

// resolvedConfigFor is the config a runtime resolves for repoRoot; a var so a
// test can supply one without files.
var resolvedConfigFor = func(repoRoot string) (*config.ResolvedConfig, error) {
	if repoRoot == "" {
		return config.ResolveGlobalConfig()
	}
	if _, err := os.Stat(repoRoot); err != nil {
		return nil, err
	}
	return config.ResolveConfig(repoRoot)
}

// HostMachine is the machine a listed host entry names.
func HostMachine(entry config.HostConfig) RuntimeMachine {
	return RuntimeMachine{Host: entry.Host}
}

// planRuntimeMachine is PlannedRuntimeMachine, as provisioning calls it; a var so
// a test can place a create without a repository.
var planRuntimeMachine = PlannedRuntimeMachine

// SetPlannedRuntimeMachineForTest makes provisioning plan with f, and returns a
// restore function. For tests outside this package.
func SetPlannedRuntimeMachineForTest(f func(opts InstanceOptions, repoRoot string) (RuntimeMachine, bool, error)) (restore func()) {
	prev := planRuntimeMachine
	planRuntimeMachine = f
	return func() { planRuntimeMachine = prev }
}
