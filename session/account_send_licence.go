package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// The licence to SEND an account, checked at the send (#7342).
//
// An account is sent to an off-box host only under a licence: the name of an
// entry in the operator's global host list (ProvisionSpec.AccountSyncHost). That
// name is decided once, when a transport is bound to a session — at provision,
// at an adoption, or when a restarted daemon rebuilds the session from its
// cleanup record — and the transport then outlives the decision by the life of
// the session. The host list does not hold still for that long. An entry can be
// removed, or edited to name another machine, while the session's transport
// keeps dialling the machine it was composed for.
//
// Three reviews in a row found that gap at a different caller, because each fix
// compared names where the decision was made. A name match proves nothing about
// where the bytes go. So the check lives here, at the send, and asks the one
// question that does: is the destination this transport dials the destination
// the licensed entry names NOW?
//
// Every sender goes through it (accountSendApplies, or accountSendLicence
// directly): the provision-time sync, the handoff's staging of the incoming
// account and its admission, and the operator's agent config that rides with
// the account. The read-back deliberately does not. Reading credentials back
// from a host and removing them is how they leave a machine the operator has
// taken out of the list, so it stays licensed by the recorded name alone
// (accountSyncApplies).

// accountSyncTarget is where an ssh transport logs in: the machine as the host
// list spells it, the port, and the remote user whose home receives the account
// scope. It is the entry's TEXT, normalised — not a resolved address — so
// re-pointing DNS behind an unchanged entry is not something this can see
// (docs/backends.md says so). It is what "the
// machine a send reaches" means here. An identity file, a host-key posture or a
// known_hosts path is not part of it: changing one changes how af proves the
// machine, not which machine it is.
type accountSyncTarget struct {
	Host string
	Port int
	User string
}

func (t accountSyncTarget) isZero() bool { return t == accountSyncTarget{} }

// String renders the target the way an operator writes it in the host list.
func (t accountSyncTarget) String() string {
	s := t.Host
	if t.User != "" {
		s = t.User + "@" + s
	}
	if t.Port != sshDefaultPort {
		s = fmt.Sprintf("%s (port %d)", s, t.Port)
	}
	return s
}

// accountSyncTargetOf reads the target out of the ssh settings a transport was
// composed from. An address af cannot parse yields the zero target, which no
// send is licensed for.
func accountSyncTargetOf(cfg config.SSHConfig) accountSyncTarget {
	// A cleanup record written before #3044 may embed a port in the host.
	host, port := normalizeLegacySSHAddress(cfg.Host, cfg.Port)
	host, port, err := resolveSSHHostPort(host, port)
	if err != nil || host == "" {
		return accountSyncTarget{}
	}
	if port == 0 {
		port = sshDefaultPort
	}
	// Two spellings of one destination must compare equal, or a spelling-only
	// edit to an entry would refuse every session already on that machine
	// (#7347):
	//   - host names compare case-insensitively;
	//   - the user is the one ssh actually logs in as, so an unset user equals
	//     the daemon's own login written out (sshLoginUser, which is also what
	//     the composed command pins).
	// Nothing else is made equal. A TRAILING DOT is always significant: whether
	// "box1.example." and "box1.example" are one machine depends on the
	// resolver's ndots and search list, which af does not control (with ndots
	// of 2 or more the relative spelling is search-expanded), and this decides
	// where credentials go. So that edit refuses, like a name against a literal
	// address, and the refusal names the edit to revert.
	host = strings.ToLower(host)
	return accountSyncTarget{Host: host, Port: port, User: sshLoginUser(cfg)}
}

// accountSendLicence reports why spec's account may not be sent over spec's
// transport right now, or nil when it may. It reads the global host list every
// time it is asked: the answer is only worth anything at the moment of the send.
//
// The host list is global-only configuration, so nothing a repository holds can
// change the answer, and a session rebuilt from its cleanup record — which
// carries no repository — can ask it too.
func accountSendLicence(spec ProvisionSpec) error {
	name := strings.TrimSpace(spec.AccountSyncHost)
	refusal := fmt.Sprintf("account %q is not sent", spec.Account.Name)
	const untouched = "Nothing was sent"
	if name == "" {
		// accountSyncApplies words this case; it is repeated so that no caller of
		// this function alone can send without a licensed name.
		return fmt.Errorf("%s: the target is not in the global host list, and credentials are only ever sent to a listed host. %s", refusal, untouched)
	}
	dialled := spec.AccountSyncTarget
	if dialled.isZero() {
		return fmt.Errorf("%s: this session did not record where its transport connects, so af cannot tell that it is the machine host %q names. %s. %s",
			refusal, name, accountInvariantBug, untouched)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("%s: the global config could not be read to check that host %q still names %s: %w. %s. Correct the global config and try again",
			refusal, name, dialled, err, untouched)
	}
	entry, listed := cfg.Hosts[name]
	if !listed {
		return fmt.Errorf("%s: host %q is no longer in the global host list, and credentials are only ever sent to a listed host. %s. Re-add it under [hosts.%s] in the global config, naming %s, and try again",
			refusal, name, untouched, name, dialled)
	}
	why := ""
	if err := config.ValidateHostEntry(name, entry); err != nil {
		why = err.Error()
	} else if entry.UseSSHConfig {
		why = "it sets use_ssh_config, so af no longer decides where a connection to it goes"
	}
	now := accountSyncTargetOf(sshConfigForHost(entry))
	if why == "" && now.isZero() {
		why = "its address cannot be read"
	}
	if why != "" {
		return fmt.Errorf("%s: host %q cannot be used for it any more: %s. %s. Correct [hosts.%s] in the global config so that it names %s again, and try again",
			refusal, name, why, untouched, name, dialled)
	}
	if now != dialled {
		return fmt.Errorf("%s: [hosts.%s] now names %s, but this session's workspace is on %s, and credentials are only ever sent to the machine a listed host names. %s. Point [hosts.%s] back at %s in the global config and try again",
			refusal, name, now, dialled, untouched, name, dialled)
	}
	return nil
}

// accountSendApplies reports whether this workspace carries an account to SEND,
// and refuses — before anything is sent — unless it holds a licence that is
// good right now for the machine its transport reaches. It is accountSyncApplies
// plus the live check; every sender asks this one, and only the read-back asks
// accountSyncApplies alone.
func (w *sandboxWorkspace) accountSendApplies() (bool, error) {
	apply, err := w.accountSyncApplies()
	if err != nil || !apply {
		return false, err
	}
	if err := accountSendLicence(w.spec); err != nil {
		return false, err
	}
	return true, nil
}
