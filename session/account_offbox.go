package session

import "strings"

// Off-box accounts on listed hosts (#5571 W7.4).
//
// `--account` and `default_accounts` are admitted on a session whose target is a
// host in the operator's global host list, and nowhere else. The licence is the
// SAME one that lets W7.3 send credentials (ProvisionSpec.AccountSyncHost): the
// ssh runtime's own host resolution (chooseListedHost). Admission, provisioning
// and re-provisioning all ask it here, so they cannot disagree about which hosts
// may receive an account — and nothing a repository wrote (its [ssh] section, a
// sandbox command, a hook) can make a target listed.

// listedAccountHost reports the listed host name a session placed by (kind, pick)
// would run on, and whether it is licensed to receive an account. Only the ssh
// kind can be: sandbox and hook targets are free-form commands af cannot list.
//
// A non-nil error means the licence could not be DETERMINED because the repo's
// project config could not be resolved (invalid TOML, or a checkout-identity
// probe that went unanswered): it is NOT the same as "not listed", and callers
// must surface it rather than report the host as unlisted (#7078). Without the
// error a listed host under an unresolvable config would be indistinguishable
// from a genuinely unlisted one, and the SSH refusal would send the operator to
// "fix" a host-list entry that is already correct.
func listedAccountHost(kind BackendKind, repoRoot, pick string) (string, bool, error) {
	if kind != BackendSSH {
		return "", false, nil
	}
	cfg, err := resolveRepoConfig(repoRoot)
	if err != nil {
		return "", false, err
	}
	name, listed, err := chooseListedHost(pick, cfg)
	if err != nil || listed == nil {
		return "", false, nil
	}
	return name, true, nil
}

// accountCarried reports whether a session of this kind placed by pick can carry
// its account: docker (a mount, #3082) or ssh on a listed host (a synced scope,
// W7.3). A non-nil error means the answer could not be determined because the
// repo's project config could not be resolved (#7078).
func accountCarried(kind BackendKind, repoRoot, pick string) (bool, error) {
	_, carried, err := accountCarriedHost(kind, repoRoot, pick)
	return carried, err
}

// accountCarriedHost reports whether a session of this kind placed by pick can
// carry its account, AND the listed host name the create resolves to when it is
// carried via a listed host (the name recordedAccountHost pins onto the row for a
// default_host pick). It is the SINGLE read of the listed-host licence a create
// makes: NewInstance resolves it once and threads the answer into the admission
// gate, the backend factory and recordedAccountHost, so the three cannot
// disagree about whether an account may be carried after a concurrent config
// change lands between two independent reads of the global and in-repo config.
//
// The host name is empty for a kind that carries by some other means (docker's
// mount), for a kind that does not carry (local, sandbox, hook, or ssh on an
// unlisted target), and for an explicit --host pick the record stores verbatim.
// A non-nil error means the licence could not be determined because the repo's
// project config could not be resolved (#7078) — the same condition
// accountCarried surfaces, so the gate can distinguish "undeterminable" from
// "determined: no" and surface the true cause.
func accountCarriedHost(kind BackendKind, repoRoot, pick string) (host string, carried bool, err error) {
	if kind.CarriesAccount() {
		return "", true, nil
	}
	return listedAccountHost(kind, repoRoot, pick)
}

// listedHostAccountReason is WHY an account is not sent to an unlisted target, stated
// as the missing GUARANTEE rather than as a claim about mechanism: on a host the
// operator listed, af syncs the credentials into the session's own scope, writes
// refreshes back, and removes them at teardown; on a target it does not control that
// way, it can give none of those guarantees.
const listedHostAccountReason = "on a host in your global host list af can give the GUARANTEE that those writes come back " +
	"(a refreshed token is written back and the credentials are removed with the session) and that the credentials stay in that " +
	"session's own scope; for any other target it cannot"

// listedHostAccountRemedy is the actionable tail of a refusal on an unlisted
// target.
func listedHostAccountRemedy() string {
	return "af sends an account only to a host in the global host list: add the host under [hosts.<name>] in the global config and pick it with --host <name>, " +
		"or omit --account to run on the host's own credentials"
}

// recordedAccountHost is the host name the session row records. It is the pick
// as made — except for an account-scoped session on a listed host reached
// through default_host, whose empty pick would otherwise be re-resolved on
// every restore and could resolve elsewhere (a repository that later gains its
// own [ssh] section would silently move the session, and its credentials, to a
// different target). Pinning the RESOLVED name makes "this session's account
// goes to this host" a recorded fact rather than a re-derivation.
//
// The resolved name is threaded in from NewInstance, which resolved the
// listed-host licence ONCE for the admission gate and the backend factory
// (accountCarriedHost). Re-deriving it here would be a THIRD independent read
// of the same licence, so a concurrent config change landing between the gate
// and the record could pin a host the gate never admitted — recording a scoped
// session on a listed host while nothing was provisioned under the account.
// Reusing the single answer is what keeps the record consistent with the gate.
func recordedAccountHost(opts InstanceOptions, resolvedListedHost string) string {
	if opts.Host != "" || strings.TrimSpace(opts.Account) == "" {
		return opts.Host
	}
	return resolvedListedHost
}
