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
	refusePushHostShared        = "push credentials are withheld on this host while a session the operator did not create runs there: `af hosts check` on the daemon box names it"
	refuseSandboxCreateLicensed = "a sandbox cannot create a session on a host the operator opted into push credentials"
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

// pushHostBlockers lists the live sessions the operator did not create whose
// runtime shares a machine with any of machines, in id order. A session with no
// recorded runtime runs nowhere and blocks nothing.
func (m *Manager) pushHostBlockers(machines []session.RuntimeMachine) []pushBlocker {
	var out []pushBlocker
	for _, inst := range m.trackedInstances() {
		if inst.OperatorCreated() {
			continue
		}
		machine, ok := instanceRuntimeMachine(inst)
		if !ok {
			continue
		}
		for _, lm := range machines {
			if lm.Shares(machine) {
				out = append(out, pushBlocker{ID: inst.ID, Title: inst.Title, Why: inst.ProvenanceText()})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
// when that host is licensed or stands for a licensed machine. The host is
// looked up exactly as the create's own chooseListedHost does (trimmed, by
// name); an unlisted one is refused by the create itself.
func (m *Manager) refuseSandboxCreateOnLicensedHost(host string) error {
	cfg := m.Config()
	if cfg == nil {
		return sandboxRefusal(refuseSandboxCreateLicensed)
	}
	entry, ok := cfg.Hosts[strings.TrimSpace(host)]
	if !ok {
		return nil
	}
	if entry.PushCredentials {
		return sandboxRefusal(refuseSandboxCreateLicensed)
	}
	target := session.RuntimeMachine{Host: entry.Host}
	for _, lm := range m.pushLicensedMachines(cfg) {
		if lm.Shares(target) {
			return sandboxRefusal(refuseSandboxCreateLicensed)
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
