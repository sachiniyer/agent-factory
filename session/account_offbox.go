package session

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
func listedAccountHost(kind BackendKind, repoRoot, pick string) (string, bool) {
	if kind != BackendSSH {
		return "", false
	}
	cfg, err := resolveRepoConfig(repoRoot)
	if err != nil {
		return "", false
	}
	name, listed, err := chooseListedHost(pick, cfg)
	if err != nil || listed == nil {
		return "", false
	}
	return name, true
}

// accountCarried reports whether a session of this kind placed by pick can carry
// its account: docker (a mount, #3082) or ssh on a listed host (a synced scope,
// W7.3).
func accountCarried(kind BackendKind, repoRoot, pick string) bool {
	if kind.CarriesAccount() {
		return true
	}
	_, ok := listedAccountHost(kind, repoRoot, pick)
	return ok
}

// listedHostAccountRemedy is the actionable tail of a refusal on an unlisted
// target.
func listedHostAccountRemedy() string {
	return "af sends an account only to a host in the global host list: add the host under [hosts.<name>] in the global config and pick it with --host <name>, " +
		"or omit --account to run on the host's own credentials"
}
