package daemon

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// While a host is push-licensed, no code the operator did not place runs on it
// (#5571 W7.8, second security review of #6433, finding 1).
//
// Sessions on one host share its ssh user, so any code running there can read a
// neighbour's 0600 callback file and connect to that neighbour's own socket: the
// neighbour's own forward, which the gate's carrier binding cannot tell apart.
// The plan accepted that residual (Q1) only because every session sharing a
// licensed host is one the operator placed. Two rules keep that true:
//
//   - a sandbox credential never creates a session on a licensed machine
//     (refuseSandboxCreateOnLicensedHost), and
//   - while ANY live session the operator did not create (sandbox-created, or of
//     unknown provenance) shares a licensed machine, that machine gets no push
//     credential for anyone (pushHostBlockers), and `af hosts check` and
//     `af doctor` name the session to remove (CheckHost).
//
// "Shares a machine" is session.RuntimeMachine.Shares, asked conservatively: by
// host name or pinned address, ignoring the login and the port.

const (
	refusePushHostShared       = "push credentials are withheld on this host while a session the operator did not place is on it: `af hosts check` on the daemon box names it"
	refusePushHostProvisioning = "push credentials are withheld on this host while a session the operator did not place is being created or re-provisioned on it: `af hosts check` on the daemon box names it"
	refusePushHostUnproven     = "push credentials are withheld because af can't establish which machine this host is: `af hosts check` on the daemon box says why"
	refuseSandboxCreateHost    = "a sandbox credential can create sessions only on a listed host the operator makes available to it"
)

// instanceRuntimeMachine is a package var so a test can place a session on a
// machine without provisioning one.
var instanceRuntimeMachine = func(i *session.Instance) (session.RuntimeMachine, bool) { return i.RuntimeMachine() }

// pushBlocker is one session that keeps a licensed machine from being answered,
// with why, and what ends it.
type pushBlocker struct {
	ID, Title, Why, Remedy string
	// inFlight marks a create or re-provision still running: waiting ends it.
	inFlight bool
}

func (b pushBlocker) String() string {
	return fmt.Sprintf("session %q (%s) %s: %s", b.Title, b.ID, b.Why, b.Remedy)
}

// pushRefusalFor is the refusal blockers earn: the in-flight kind when every
// one is in flight, else the placed kind.
func pushRefusalFor(blockers []pushBlocker) error {
	for _, b := range blockers {
		if !b.inFlight {
			return sandboxRefusal(refusePushHostShared)
		}
	}
	return sandboxRefusal(refusePushHostProvisioning)
}

const (
	remedyKill     = "remove it with `af sessions kill`; archiving it isn't enough, because a process a hook detached outlives the archive"
	remedyWait     = "wait for it to finish; it is named here again if it still blocks"
	remedyKillOrNS = "make its host name resolve, or remove it with `af sessions kill`"
)

// trackedInstances is every row the manager tracks, collected under m.mu and
// returned for reading outside it, so no caller holds the manager lock across an
// instance lock.
func (m *Manager) trackedInstances() []*session.Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*session.Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		if inst != nil {
			out = append(out, inst)
		}
	}
	return out
}

// pushHostBlockers lists, in id order, each session that may run code the
// operator did not place on a machine sharing one of machines:
//   - a row the operator did not create (sandbox-created, or of unknown
//     provenance), or one on a repository's own [ssh] target, which the
//     repository chose;
//   - a create of that kind still provisioning, whose hooks run before its row is
//     tracked (holdInflightCreate);
//   - any such row being re-provisioned, whose machine is Unknown until the
//     replacement is recorded, so it shares every machine (fail closed).
//
// A session with no recorded runtime runs nowhere and blocks nothing, and neither
// does an archive af confirmed stopped while keeping its workspace (Codex P2 on
// #6433): its recorded runtime only lets a later kill reap the directory.
//
// The two reads are ordered so a create finishing mid-scan is never missed
// (Codex P1 on #6433): the in-flight creates are read FIRST, and the tracked rows
// after. A create commits its row and only then releases its hold, so a create
// missing from the first read had committed before it, and is in the second.
// Both reads come before any slow comparison (Shares may resolve names).
func (m *Manager) pushHostBlockers(machines []session.RuntimeMachine) []pushBlocker {
	inflight := m.pushCreds.inflightCreates()
	testHookBetweenBlockerReads()
	tracked := m.trackedInstances()
	var out []pushBlocker
	seen := map[string]bool{}
	add := func(b pushBlocker, machine session.RuntimeMachine) {
		if seen[b.ID] {
			return
		}
		for _, lm := range machines {
			if lm.Shares(machine) {
				seen[b.ID] = true
				out = append(out, b)
				return
			}
		}
	}
	for _, inst := range tracked {
		// Every row that stood on the machine counts until it is KILLED, archived
		// or not: neither an archive nor a reap ends a process a hook detached.
		machine, ok := instanceRuntimeMachine(inst)
		if !ok || (inst.OperatorCreated() && !machine.RepoChosen) {
			continue
		}
		b := pushBlocker{ID: inst.ID, Title: inst.Title, Why: "was " + inst.ProvenanceText(), Remedy: remedyKill}
		if inst.OperatorCreated() {
			b.Why = "is on a repository's own [ssh] target, which the repository chose"
		}
		if reason, kind := machine.Unproven(); reason != "" {
			b.Why += ", and counts as every machine because " + reason
			switch kind {
			case session.UnprovenInFlight:
				b.Remedy, b.inFlight = remedyWait, true
			case session.UnprovenName:
				b.Remedy = remedyKillOrNS
			}
		}
		add(b, machine)
	}
	for id, c := range inflight {
		add(pushBlocker{ID: id, Title: c.title, Why: "is still being created there, by another session's af or onto a repository's own [ssh] target",
			Remedy: remedyWait, inFlight: true}, c.machine)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// testHookBetweenBlockerReads runs between pushHostBlockers' two reads. Tests
// only.
var testHookBetweenBlockerReads = func() {}

// inflightCreate is a create the operator did not place, from before NewInstance
// until its row is tracked.
type inflightCreate struct {
	title   string
	machine session.RuntimeMachine
}

// plannedRuntimeMachine is a package var so a test can plan a create's machine
// without a repository.
var plannedRuntimeMachine = session.PlannedRuntimeMachine

// holdInflightCreate records a create as a blocker for as long as it provisions,
// when the operator did not place it: a sandbox credential asked for it, or it
// goes to a repository's own [ssh] target (or one af cannot resolve). Its machine
// is the one the runtime will resolve. The returned func releases it, once the row
// is tracked or the create has failed.
func (m *Manager) holdInflightCreate(id, title string, opts session.InstanceOptions, repoRoot string, bySandbox bool) (release func()) {
	planned, offBox := plannedRuntimeMachine(opts, repoRoot)
	if !offBox || (!bySandbox && !planned.RepoChosen) {
		return func() {}
	}
	return m.pushCreds.holdInflight(id, inflightCreate{title: title, machine: planned})
}

// listedHostMachines is every machine the listed host name stands for: the
// configured destination, and the machine each live session placed on it
// recorded (the address its ssh runtime dialed and pinned).
func (m *Manager) listedHostMachines(name string, entry config.HostConfig) []session.RuntimeMachine {
	out := []session.RuntimeMachine{{Host: entry.Host}}
	for _, inst := range m.trackedInstances() {
		if p, ok := instancePushPlacement(inst); ok && p.ListedHost == name {
			out = append(out, p.Machine)
		}
	}
	return out
}

// pushLicensedMachines is every machine the config opts into push credentials.
func (m *Manager) pushLicensedMachines(cfg *config.Config) []session.RuntimeMachine {
	if cfg == nil {
		return nil
	}
	var out []session.RuntimeMachine
	for name, entry := range cfg.Hosts {
		if entry.PushCredentials {
			out = append(out, m.listedHostMachines(name, entry)...)
		}
	}
	if cfg.SandboxPushCredentials && strings.TrimSpace(cfg.SandboxSSH) != "" {
		out = append(out, session.RuntimeMachine{SandboxSSH: cfg.SandboxSSH})
	}
	return out
}

// refuseSandboxCreateOnLicensedHost is the create half of the rule: a sandbox
// credential's create names a listed host (checkSandboxCreate), and is refused
// when that host is licensed or may stand for a licensed machine (Shares, which
// refuses what it cannot show to differ). The host is looked up exactly as the
// create's own chooseListedHost does (trimmed, by name).
//
// The refusal is one generic text, the same as for a host that is not listed
// at all, so a sandbox cannot use it to learn which names are licensed.
func (m *Manager) refuseSandboxCreateOnLicensedHost(host string) error {
	cfg := m.Config()
	if cfg == nil {
		return sandboxRefusal(refuseSandboxCreateHost)
	}
	entry, ok := cfg.Hosts[strings.TrimSpace(host)]
	if !ok || entry.PushCredentials {
		return sandboxRefusal(refuseSandboxCreateHost)
	}
	target := session.RuntimeMachine{Host: entry.Host}
	for _, lm := range m.pushLicensedMachines(cfg) {
		if lm.Shares(target) {
			return sandboxRefusal(refuseSandboxCreateHost)
		}
	}
	return nil
}

// pushBlockerTexts renders blockers for a CheckHost result.
func pushBlockerTexts(blockers []pushBlocker) []string {
	if len(blockers) == 0 {
		return nil
	}
	out := make([]string, 0, len(blockers))
	for _, b := range blockers {
		out = append(out, b.String())
	}
	return out
}

// fillPushCredentialBlockers adds, to each licensed host's result and to the
// response for a licensed sandbox.ssh target, the sessions keeping it from being
// answered. It reads the manager's rows, never the probe, so it reports a host
// whose probe was skipped or failed too.
func (m *Manager) fillPushCredentialBlockers(cfg *config.Config, resp *CheckHostResponse) {
	if cfg == nil {
		return
	}
	for i := range resp.Hosts {
		entry, ok := cfg.Hosts[resp.Hosts[i].Name]
		if !ok || !entry.PushCredentials {
			continue
		}
		resp.Hosts[i].PushCredentialBlockers = m.pushBlockerReport(session.RuntimeMachine{Host: entry.Host}, m.listedHostMachines(resp.Hosts[i].Name, entry))
	}
	if cfg.SandboxPushCredentials && strings.TrimSpace(cfg.SandboxSSH) != "" {
		target := session.RuntimeMachine{SandboxSSH: cfg.SandboxSSH}
		resp.SandboxPushCredentialBlockers = m.pushBlockerReport(target, []session.RuntimeMachine{target})
	}
}

// pushBlockerReport is what CheckHost says withholds a licensed target: first
// the target itself when af can't establish which machine it is, then each
// session sharing it.
func (m *Manager) pushBlockerReport(target session.RuntimeMachine, machines []session.RuntimeMachine) []string {
	var out []string
	if reason, _ := target.Unproven(); reason != "" {
		out = append(out, "af can't establish which machine this is, because "+reason+": fix that, and push credentials resume")
	}
	return append(out, pushBlockerTexts(m.pushHostBlockers(machines))...)
}
