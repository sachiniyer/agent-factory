package api

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session"
)

// accountAutoSkewRefusal is the CLI half of #4404's version-skew contract: an
// omitted --account asks a routing daemon to pick from the account pool, but a
// PRE-router daemon's gob decoder silently drops account_auto and applies its
// legacy contract — a configured default_accounts entry WITHOUT the router's
// wall check, or the ambient identity while a logged-in pool sits unrouted. The
// response cannot tell skew from honor (a default applied by an old daemon reads
// exactly like the same default the new contract prefers), so the capability is
// checked before the create, and a refusal costs nothing.
//
// The decision points, in order, and which way each one fails:
//
//  1. An agent with no account registry, or a backend that cannot carry an
//     account (ssh/sandbox/hook — the daemon's router deliberately leaves those
//     on the legacy contract too): both daemons produce the same identity, so
//     there is no skew to refuse (#4404 review). The backend is resolved with
//     the router's own predicate, flag first and then the repo's `backend` key.
//  2. Ping says the daemon routes: proceed. Ping is the capability source, not
//     ListAccounts, because ListAccounts fails outright on ONE unreadable
//     account — which a routing daemon's create simply routes around — and an
//     unknown answer there would refuse creates a current daemon handles.
//  3. Ping fails: the capability is UNKNOWN, and proceeding would let an old
//     daemon run the legacy contract silently, so refuse (#4404 review). The
//     create would reach the same daemon over the same transport, so this
//     costs a real create only a transient blip — and the message says to
//     retry or pin.
//  4. Ping says the daemon predates routing: refuse only when the outcome can
//     differ — a configured default or a logged-in account. With neither, both
//     contracts land on ambient. A ListAccounts failure HERE leaves that
//     unknown against a daemon known to be old, so it refuses too.
func accountAutoSkewRefusal(program, workspace, backend string, inPlace bool) error {
	agent := sessionenv.AgentForCommand(program)
	if agent == "" {
		return nil
	}
	if _, ok := sessionenv.SupportsAccounts(agent); !ok {
		return nil
	}
	kind, err := session.BackendKindFor(session.InstanceOptions{
		Backend: session.BackendKind(backend),
		InPlace: inPlace,
	}, workspace)
	if err != nil || !kind.LaunchesWithAccount() {
		// An unresolvable backend is refused by NewInstance on either daemon.
		return nil
	}
	const pin = "pin the identity yourself — --account <name> for a registered account, --account \"\" for ambient"
	ping, err := pingDaemonCapabilities()
	if err != nil {
		return fmt.Errorf("omitting --account asks the daemon to pick the account, but af could not confirm the "+
			"running daemon supports that: %w. Retry, or %s", err, pin)
	}
	if ping.PoolRouting {
		return nil
	}
	accounts, err := listAccountsViaDaemon(daemon.ListAccountsRequest{Agent: agent, RepoPath: workspace})
	if err != nil {
		return fmt.Errorf("omitting --account asks the daemon to pick the account, but the running daemon predates "+
			"pool routing and its %s accounts could not be listed to tell whether that changes this session's "+
			"identity: %w. Upgrade the daemon (af daemon restart after upgrading) and recreate, or %s",
			agent, err, pin)
	}
	routable := false
	for _, entry := range accounts.Entries {
		if entry.Agent == agent && entry.LoggedIn {
			routable = true
			break
		}
	}
	def := strings.TrimSpace(accounts.Defaults[agent])
	if !routable && def == "" {
		return nil
	}
	would := "launch on the ambient identity while logged-in accounts sit unrouted"
	if def != "" {
		would = fmt.Sprintf("apply the configured default %q without the router's wall check", def)
	}
	return fmt.Errorf("omitting --account asks the daemon to pick the account, but the running daemon predates "+
		"pool routing and would %s. Upgrade the daemon (af daemon restart after upgrading) and recreate, or %s",
		would, pin)
}
