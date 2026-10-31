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
	refusePushHostShared    = "push credentials are withheld on this host while a session the operator did not create runs there: `af hosts check` on the daemon box names it"
	refuseSandboxCreateHost = "a sandbox credential can create sessions only on a listed host the operator makes available to it"
)

// instanceRuntimeMachine is a package var so a test can place a session on a
// machine without provisioning one.
var instanceRuntimeMachine = func(i *session.Instance) (session.RuntimeMachine, bool) { return i.RuntimeMachine() }

// pushBlocker is one live session that keeps a licensed machine from being
// answered.
type pushBlocker struct {
	ID, Title, Why string
}

func (b pushBlocker) String() string {
	return fmt.Sprintf("session %q (%s) is %s: remove it (af sessions kill) to restore push credentials here", b.Title, b.ID, b.Why)
}

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
func (m *Manager) pushHostBlockers(machines []session.RuntimeMachine) []pushBlocker {
	var out []pushBlocker
	add := func(id, title, why string, machine session.RuntimeMachine) {
		for _, lm := range machines {
			if lm.Shares(machine) {
				out = append(out, pushBlocker{ID: id, Title: title, Why: why})
				return
			}
		}
	}
	for _, inst := range m.trackedInstances() {
		machine, ok := instanceRuntimeMachine(inst)
		if !ok || inst.StoppedPreservedArchive() || (inst.OperatorCreated() && !machine.RepoChosen) {
			continue
		}
		why := inst.ProvenanceText()
		if inst.OperatorCreated() {
			why = "on a repository's own [ssh] target, which the repository chose"
		}
		if machine.Unknown {
			why += ", and being re-provisioned onto a machine af can't name yet"
		}
		add(inst.ID, inst.Title, why, machine)
	}
	for id, c := range m.pushCreds.inflightCreates() {
		add(id, c.title, "still being created there, by another session's af or onto a repository's own [ssh] target", c.machine)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

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
		resp.Hosts[i].PushCredentialBlockers = pushBlockerTexts(m.pushHostBlockers(m.listedHostMachines(resp.Hosts[i].Name, entry)))
	}
	if cfg.SandboxPushCredentials && strings.TrimSpace(cfg.SandboxSSH) != "" {
		resp.SandboxPushCredentialBlockers = pushBlockerTexts(m.pushHostBlockers([]session.RuntimeMachine{{SandboxSSH: cfg.SandboxSSH}}))
	}
}
