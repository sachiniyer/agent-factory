package daemon

// The accounts wire plane (#3384/#3385).
//
// Split out of control_types.go, which had reached the 1000-line production
// limit (#1145). Accounts are a self-contained surface — one entry shape and
// three request/response pairs, none of them referenced by the session or task
// types — so they are the natural seam rather than an arbitrary cut.
//
// The rule that governs every struct here: none of them carries credential
// material, and none of them ever will. An account is a DIRECTORY af chooses;
// the material inside it belongs to the agent that wrote it. af reports where
// the directory is and whether the agent's own file is present in it, by stat.
// Any field that would carry a token belongs in a different design (#3051).
//
// Plain values throughout, like every other request on this plane: the control
// socket is net/rpc gob, which elides zero-value fields, so an optional pointer
// would arrive as nil (#1700).

// AccountEntry is one registered agent account, as every surface reports it.
//
// It is defined HERE, on the wire plane, and used unchanged by the CLI's
// `af accounts list --json` (which reads the local home directly, no daemon
// involved) as well as by the daemon's ListAccounts. One struct rather than two
// with the same field names: a `logged_in` that meant different things — or
// appeared on one surface and not the other — is precisely the drift that makes
// a UI and a CLI disagree about whether an account works.
//
// It carries no credential and never will. Dir is the DIRECTORY, and LoggedIn is
// the presence of the agent's own credential file under it, by stat.
type AccountEntry struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
	// Dir is the account's credential directory — the thing an operator points a
	// login flow at, which is why omitting it would make the JSON strictly less
	// useful than the human output.
	Dir string `json:"dir"`
	// RegistrationOnly is true while a session cannot yet be scoped to this
	// agent's accounts — af has verified that its credential root relocates, not
	// that the account boundary can prove how af launches it.
	//
	// Always emitted, never omitempty: a caller reading this to decide whether to
	// start a session needs the difference between "false" and "this af is too old
	// to say", and an omitted false is the same bytes as a missing field (#3609
	// review).
	RegistrationOnly bool `json:"registration_only"`
	// LoggedIn is whether the agent's own login flow has left its credential in
	// this account (#3384). It is read by STAT — af never opens the file — so it
	// answers "this account has an identity", not "this identity is valid": a
	// revoked or expired credential is still a credential on disk, and only the
	// agent can say otherwise.
	//
	// Always emitted, for the same reason as RegistrationOnly.
	LoggedIn bool `json:"logged_in"`
}

// ListAccountsRequest reads the registered accounts on the DAEMON's host, which
// is where the credential directories are. An empty Agent means every agent that
// supports accounts.
type ListAccountsRequest struct {
	Agent string `json:"agent"`
	// RepoPath sharpens Defaults, which is a PER-PROJECT fact even though the
	// registry itself is not: `default_accounts` admits the machine-local
	// per-project layer, so "which account would a create use here" depends on
	// which project "here" is (#3386).
	//
	// Optional, exactly as ListProgramsRequest.RepoPath is: the registry and the
	// roster are global, so a caller with no project in hand still gets a useful
	// answer — just one whose Defaults come from the global layer alone.
	RepoPath string `json:"repo_path,omitempty"`
}

// ListAccountsResponse carries the accounts and the roster they can be created
// in.
//
// Agents is not derivable from Entries: a fresh install has no accounts at all,
// and a UI still has to offer the agents one can be registered for. Sending it
// is what lets a client render the register form without a second round trip or
// a hardcoded list that goes stale the day a fourth agent is verified.
type ListAccountsResponse struct {
	Entries []AccountEntry `json:"entries"`
	Agents  []string       `json:"agents"`
	// Defaults maps an agent to the account a create with NO explicit account
	// would run as for RepoPath — the `default_accounts` key resolved through the
	// same precedence the create applies (#3386). An agent with no configured
	// default is absent, which means the ambient identity.
	//
	// It exists so a picker can PRESELECT what the daemon is going to do instead
	// of sending nothing and letting the daemon fill it in silently — the
	// complaint #3386 opens with. Clients render it; they never compute it, for
	// the reason ListPrograms.Default exists: a second implementation of the
	// precedence is a second answer, and only one of them is the one the create
	// uses.
	//
	// It is deliberately NOT validated here. A default naming an account that is
	// not registered is reported as configured, and the create refuses it by name
	// — dropping it from this map would hide the misconfiguration behind an
	// "ambient identity" the picker would then be lying about.
	Defaults map[string]string `json:"defaults,omitempty"`
}

// RegisterAccountRequest creates an account's credential directory on the
// daemon's host. Idempotent: registering an existing account reports the same
// directory and touches nothing inside it.
type RegisterAccountRequest struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
}

// RegisterAccountResponse is the account as it now stands, plus what the
// operator must be told before they log into it.
type RegisterAccountResponse struct {
	Entry AccountEntry `json:"entry"`
	// Notices are what the agent's credential-root variable relocates, and
	// anything af could not verify about the account directory. Never about a
	// credential's contents.
	Notices []string `json:"notices,omitempty"`
}

// AccountLoginRequest asks the daemon to open an agent's OWN login flow in a
// bare tmux session scoped to one registered account (#3384) — no Instance, no
// worktree, no repo, and no row in the session list. The account is registered
// first if it does not exist yet, which makes the verb idempotent from the
// caller's side.
type AccountLoginRequest struct {
	// Agent is the agent whose login flow to run. Only agents with a VERIFIED
	// login command are accepted; af never guesses `<agent> login` (#3057).
	Agent string `json:"agent"`
	// Name is the account name, in that agent's namespace.
	Name string `json:"name"`
}

// AccountLoginResponse describes the login pane well enough to attach to it and
// to report the outcome without a second round trip.
//
// It carries no credential and never will: Dir is the DIRECTORY af pointed the
// agent's credential-root variable at, and LoggedIn is derived from the presence
// of the agent's own artifact under it (by stat, never by reading it). af sets a
// directory and hands over the terminal; the material is the agent's.
type AccountLoginResponse struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
	// Dir is the account's credential directory.
	Dir string `json:"dir"`
	// Program is the exact invocation the pane runs — the agent's own login
	// command — so a caller can show what af is running rather than assert it.
	Program string `json:"program"`
	// SessionName is the tmux session to attach to, as tmux knows it. EMPTY when
	// Finished is true: the flow ended before af could hand over a terminal, and
	// there is nothing left to attach to.
	SessionName string `json:"session_name"`
	// SocketPath is the absolute tmux socket the pane lives on, so a client can
	// pin it with `tmux -S <path> attach-session` (#2019). A plain string for the
	// same gob reason as SpawnConfigAgentResponse's: empty is a legitimate value
	// — the attach then falls back to the default socket — so it must transmit as
	// "" rather than be laundered into nil.
	SocketPath string `json:"socket_path"`
	// Reused is true when this call joined a login flow that was already open for
	// this account rather than starting a competing one.
	Reused bool `json:"reused"`
	// Finished is true when the agent's login command ran to completion before af
	// could hand over the terminal — `codex login` against an account that
	// already holds a credential does this.
	Finished bool `json:"finished"`
	// LoggedIn is the account's artifact state as OBSERVED AT THIS CALL, before
	// the flow it opened has had a chance to change it (except when Finished,
	// where it is the state after). A caller that wants the state afterwards asks
	// again once the flow ends.
	LoggedIn bool `json:"logged_in"`
	// Notices are the things the operator has to be told before using this
	// account — what the agent's credential-root variable relocates, and anything
	// af could not verify. Never a warning about a credential's contents.
	Notices []string `json:"notices,omitempty"`
}
