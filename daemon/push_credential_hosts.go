package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// While a host is push-licensed, no code the operator did not place may have run
// as its ssh user (#5571 W7.8, security reviews of #6433 and the design steer
// that followed them).
//
// Sessions on one host share its ssh user, so code running there can read a
// neighbour's 0600 callback file and connect to that neighbour's own socket. And
// residue outlives a session: a process a hook detached survives archive, reap,
// replacement and kill. So the rule is not about session ROWS at all. Each
// placement of code the operator did not place TAINTS the machine and user it
// goes to: a sandbox create, a repository-target create, a failed create, a
// re-provision of such a session. The taint is written before any of that code
// can run (session.PlacementRecorder), it is monotone, and only an explicit
// operator reset (`af hosts reset-trust`) clears it (push_credential_taint.go).
// Here are the rules that read it:
//
//   - a sandbox credential never creates a session on a licensed machine
//     (refuseSandboxCreateOnLicensedHost);
//   - a tainted machine gets no push credential (authorizePushCredential);
//   - licensing a tainted machine is refused up front (refuseLicensingTainted);
//   - `af hosts check` and `af doctor` show each taint, with the session that
//     made it as context (fillPushCredentialBlockers).

const (
	refusePushHostTainted   = "push credentials are withheld: code the operator didn't place has run as this host's user. `af hosts check` on the daemon box says which, and `af hosts reset-trust` clears it once the user is recreated or wiped"
	refusePushHostUnproven  = "push credentials are withheld because af can't establish which machine this host is: `af hosts check` on the daemon box says why"
	refusePushTaintsLoading = "push credentials are withheld while af records where its restored sessions run; try again in a moment"
	refuseSandboxCreateHost = "a sandbox credential can create sessions only on a listed host the operator makes available to it"
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

// pushLicensedMachines is every machine the config opts into push credentials.
func pushLicensedMachines(cfg *config.Config) []session.RuntimeMachine {
	if cfg == nil {
		return nil
	}
	var out []session.RuntimeMachine
	for _, entry := range cfg.Hosts {
		if entry.PushCredentials {
			out = append(out, session.HostMachine(entry))
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
	target := session.HostMachine(entry)
	for _, lm := range pushLicensedMachines(cfg) {
		if lm.Shares(target) {
			return sandboxRefusal(refuseSandboxCreateHost)
		}
	}
	return nil
}

// fillPushCredentialBlockers adds, to each listed host's result and to the
// response for the sandbox.ssh target, what keeps it from push credentials: for
// a licensed target af can't identify, that; then every taint on its machine
// and user, whether it is licensed yet or not (a tainted machine can't be
// licensed until it is reset). It reads the daemon's records, never the probe,
// so it reports a host whose probe was skipped or failed too.
func (m *Manager) fillPushCredentialBlockers(cfg *config.Config, resp *CheckHostResponse) {
	if cfg == nil {
		return
	}
	for i := range resp.Hosts {
		entry, ok := cfg.Hosts[resp.Hosts[i].Name]
		if !ok {
			continue
		}
		resp.Hosts[i].PushCredentialBlockers = m.pushTrustReport(resp.Hosts[i].Name, session.HostMachine(entry), entry.PushCredentials)
	}
	if strings.TrimSpace(cfg.SandboxSSH) != "" {
		resp.SandboxPushCredentialBlockers = m.pushTrustReport(sandboxTrustTarget, session.RuntimeMachine{SandboxSSH: cfg.SandboxSSH}, cfg.SandboxPushCredentials)
	}
}

// pushTrustReport is what CheckHost says withholds push credentials from target.
func (m *Manager) pushTrustReport(name string, target session.RuntimeMachine, licensed bool) []string {
	id := target.Identity()
	var out []string
	if licensed && id.Unproven != "" {
		out = append(out, "af can't establish which machine this is, because "+id.Unproven+": fix that, and push credentials resume")
	}
	shown := map[string]bool{}
	for _, t := range m.pushCreds.taintsFor(id) {
		shown[t.ID] = true
		out = append(out, fmt.Sprintf("%s. Recreate or wipe this machine's users, then run `af hosts reset-trust %s`", t, name))
	}
	// An entry af couldn't place on an identified machine blocks licensing every
	// host, so every host names it.
	if thin, err := m.pushCreds.taints.unconfirmed(); err == nil {
		for _, t := range thin {
			if !shown[t.ID] {
				out = append(out, fmt.Sprintf("%s. No host can be opted into push credentials until it is reset: `af hosts reset-trust --entry %s`", t, t.ID))
			}
		}
	}
	return out
}
