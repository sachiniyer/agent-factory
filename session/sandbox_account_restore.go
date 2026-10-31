package session

import "github.com/sachiniyer/agent-factory/log"

// restoredAccount is what the row remembers about a session's account: enough to
// name it again, never a credential.
type restoredAccount struct {
	Name  string
	Agent string
	// Program is the row's agent command, which the handoff's attribution proof
	// runs against.
	Program string
}

// accountSyncHost is the listed host that licensed this provision's synced
// account, recorded in the cleanup handle so a reattach can rebuild the account
// workspace. Empty without a synced account.
func (p *sandboxProvisioner) accountSyncHost() string {
	if p == nil || p.account == nil || p.account.spec.Account.Dir == "" {
		return ""
	}
	return p.account.spec.AccountSyncHost
}

// restoreAccountWorkspace rebuilds the account workspace of a session reattached
// after a daemon restart (#5948), so account handoff, limit failover and the
// teardown's credential removal apply to it again.
//
// It is rebuilt from the row alone — the account's name and agent, the listed
// host that licensed it, the session directory — and touches no credential: the
// account is looked up in the local registry, nothing is read from or sent to the
// host. It carries NO sync-in baseline (syncedVersion stays nil), so a write-back
// from this process can never replace the registry's copy: every file the host
// holds reads as "this process holds no baseline" and is skipped, exactly as
// writeBackAccountFrom documents. A swap rotates from there and records the new
// account's own baseline.
//
// Without a recorded account or host, or when the account no longer resolves in
// the registry, nothing is rebuilt and the session behaves as it did before: no
// off-box handoff.
func (p *sandboxProvisioner) restoreAccountWorkspace(account restoredAccount, host string) {
	if account.Name == "" || account.Agent == "" || host == "" || p.sessionDir == "" {
		return
	}
	acct, err := selectAccountInNamespace(account.Agent, account.Name)
	if err != nil {
		log.WarningLog.Printf("session %q: not restoring its synced account %q after the restart: %v", p.spec.Title, account.Name, err)
		return
	}
	p.spec.Account = acct
	p.spec.Program = account.Program
	p.spec.AccountSyncHost = host
	p.account = &sandboxWorkspace{shell: p, spec: p.spec, SessionDir: p.sessionDir, restored: true}
}
