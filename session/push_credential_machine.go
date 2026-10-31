package session

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// Which machine a runtime runs on (#5571 W7.8, security reviews of #6433). The
// push-credential host rule asks one question of it, Shares, and asks it
// CONSERVATIVELY: where two runtimes cannot be shown to run on different machines,
// they are treated as one, because sessions on one machine share its ssh user.

// RuntimeMachine names the machine an off-box runtime runs on, from the handle
// its provision recorded: the ssh destination and the address af actually
// dialed (the ssh runtime pins it, ssh_machine_pin.go), or the free-form
// sandbox.ssh command.
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
	// Unknown marks a runtime whose machine af cannot name right now (a
	// re-provision or create in flight). It shares every machine.
	Unknown bool
}

// Shares reports whether two runtimes may run on the same machine. They do when
// either is Unknown, when any canonical name of one matches one of the other's
// (a short name matches the first label of a longer one), or when their
// addresses meet, as configured, pinned or resolved. A name that does not
// resolve cannot be shown to differ from anything, so it shares every machine;
// so does a sandbox.ssh command af cannot read a destination from. A runtime
// with nothing recorded runs nowhere and shares nothing.
//
// Not detected: two literal addresses of one multi-homed machine that no name
// links, and an /etc/hosts or DNS answer that differs from what the session's
// ssh dialed when it was placed (its pinned address is compared too).
func (m RuntimeMachine) Shares(o RuntimeMachine) bool {
	a, b := m.identity(), o.identity()
	if a.empty() || b.empty() {
		return false
	}
	if a.unknown || b.unknown {
		return true
	}
	for x := range a.names {
		for y := range b.names {
			if x == y || shortNameOf(x, y) || shortNameOf(y, x) {
				return true
			}
		}
	}
	for addr := range a.addrs {
		if b.addrs[addr] {
			return true
		}
	}
	return false
}

// machineIdentity is a RuntimeMachine's canonical names and addresses.
type machineIdentity struct {
	names   map[string]bool
	addrs   map[netip.Addr]bool
	unknown bool
}

func (id machineIdentity) empty() bool {
	return !id.unknown && len(id.names) == 0 && len(id.addrs) == 0
}

func (m RuntimeMachine) identity() machineIdentity {
	id := machineIdentity{names: map[string]bool{}, addrs: map[netip.Addr]bool{}}
	if m.Unknown {
		id.unknown = true
		return id
	}
	hosts := []string{m.Host, m.Addr}
	if strings.TrimSpace(m.SandboxSSH) != "" {
		dest, ok := sandboxSSHDestination(m.SandboxSSH)
		if !ok {
			id.unknown = true
			return id
		}
		hosts = append(hosts, dest)
	}
	for _, h := range hosts {
		id.add(h)
	}
	return id
}

// add records h: an address in canonical form (IPv4-mapped IPv6 unmapped, zone
// dropped), or a canonical name with every address it resolves to. A name that
// does not resolve makes the identity unknown.
func (id *machineIdentity) add(h string) {
	h = canonicalMachineName(h)
	if h == "" {
		return
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		id.addrs[addr.Unmap().WithZone("")] = true
		return
	}
	id.names[h] = true
	addrs, err := resolveMachine(h)
	if err != nil || len(addrs) == 0 {
		id.unknown = true
		return
	}
	for _, a := range addrs {
		id.addrs[a] = true
	}
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

// sandboxSSHDestination is the destination host a sandbox.ssh command names: the
// first non-option argument of an ssh invocation, or a -o HostName= override,
// without a user@, an ssh:// or a :port. ok is false for a command that is not
// a plain ssh invocation (a wrapper script), whose destination af cannot read.
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
	flushMachineLookups()
	return func() {
		machineLookup = prev
		flushMachineLookups()
	}
}

func flushMachineLookups() {
	machineLookups.mu.Lock()
	machineLookups.m = map[string]machineLookupEntry{}
	machineLookups.mu.Unlock()
}

// RuntimeMachine reports the machine this session's off-box runtime runs on,
// for any ssh runtime (a listed host or a repository's [ssh] target) and the
// free-form sandbox target. While a re-provision is replacing the runtime, the
// machine is Unknown. ok is false when no runtime is recorded and none is being
// provisioned: a local, docker or hook session, or one whose runtime has been
// reaped.
func (i *Instance) RuntimeMachine() (RuntimeMachine, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.provisioning > 0 {
		// The repository chose the placement unless the row says the operator
		// did: the free-form target, or a named listed host.
		_, sandbox := i.backend.(*sandboxBackend)
		_, ssh := i.backend.(*sshBackend)
		return RuntimeMachine{Unknown: true, RepoChosen: !sandbox && !(ssh && strings.TrimSpace(i.Host) != "")}, true
	}
	provider, ok := i.backend.(runtimeCleanupProvider)
	if !ok {
		return RuntimeMachine{}, false
	}
	m := machineOf(provider.runtimeCleanupData())
	return m, !m.identity().empty()
}

// beginProvisioning marks a re-provision in flight; the returned func ends it.
func (i *Instance) beginProvisioning() (end func()) {
	i.mu.Lock()
	i.provisioning++
	i.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			i.mu.Lock()
			i.provisioning--
			i.mu.Unlock()
		})
	}
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

// PlannedRuntimeMachine is the machine a create with opts would provision onto,
// resolved before it provisions, the way the runtime itself will resolve it:
// the listed host chooseListedHost picks (named, or default_host), the
// repository's own [ssh] target, or the free-form sandbox.ssh target. offBox is
// false for a runtime that gets no af callback (local, docker, hook). A target
// that cannot be resolved here is Unknown.
func PlannedRuntimeMachine(opts InstanceOptions, repoRoot string) (m RuntimeMachine, offBox bool) {
	kind, err := BackendKindFor(opts, repoRoot)
	if err != nil {
		return RuntimeMachine{Unknown: true, RepoChosen: true}, true
	}
	if !kind.InjectsSandboxCallback() {
		return RuntimeMachine{}, false
	}
	cfg, err := resolvedConfigFor(repoRoot)
	if err != nil || cfg == nil {
		return RuntimeMachine{Unknown: true, RepoChosen: true}, true
	}
	if kind == BackendSandbox {
		return RuntimeMachine{SandboxSSH: cfg.SandboxSSH}, true
	}
	name, listed, err := chooseListedHost(opts.Host, cfg)
	switch {
	case err != nil:
		return RuntimeMachine{Unknown: true, RepoChosen: true}, true
	case name != "" && listed != nil:
		return RuntimeMachine{Host: listed.Host}, true
	case cfg.SSH != nil && strings.TrimSpace(cfg.SSH.Host) != "":
		return RuntimeMachine{Host: cfg.SSH.Host, RepoChosen: true}, true
	}
	return RuntimeMachine{Unknown: true, RepoChosen: true}, true
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
