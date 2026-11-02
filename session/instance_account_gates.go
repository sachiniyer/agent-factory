package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// Account-admission gates for NewInstance: which backends may carry a
// registered credential account, which agents may hold one, and how a
// sourced (non-requested) account is resolved into its registry directory.
// Extracted from instance_factory.go to keep both files under the 1000-line
// lint limit (#1145).

// refuseOffBoxAccount rejects an account-scoped session on a backend that cannot
// carry the account. For ssh, sandbox and hook this is a DECISION, not a gap
// (#3103) — see offBoxAccountRefusal for each backend's reason.
//
// It still gates on the backend being NOT local rather than on a list of remote
// kinds, and #3082 did not weaken that: a backend added later is off-box until
// someone proves otherwise, so the default for anything new remains refusal
// rather than silent ambient credentials. What changed is that docker can now
// PROVE it, so it is exempted by name.
func refuseOffBoxAccount(opts InstanceOptions) error {
	if strings.TrimSpace(opts.Account) == "" {
		return nil
	}
	kind := opts.Backend
	if kind == "" {
		kind = BackendLocal
	}
	return refuseOffBoxAccountForKind(opts, kind)
}

func refuseOffBoxAccountForKind(opts InstanceOptions, kind BackendKind) error {
	if strings.TrimSpace(opts.Account) == "" {
		return nil
	}
	if kind == BackendLocal || kind.CarriesAccount() {
		return nil
	}
	return accountRefusalWithSource(opts,
		fmt.Errorf("account %q cannot be used with the %s backend: %s", opts.Account, kind, offBoxAccountRefusal(kind)))
}

// offBoxAccountRefusal appends only backend-specific facts to the shared
// AccountWriteBackRationale. The common reason must remain location- and
// mechanism-neutral because hook may use shared storage or run on the daemon
// host; see CarriesAccount.
const offBoxRoundTripReason = AccountWriteBackRationale +
	" af refuses this BY DESIGN and not as pending work: what is missing is the GUARANTEE that " +
	"those writes come back, and af will not present an account as writable without one."

func offBoxAccountRefusal(kind BackendKind) string {
	switch kind {
	case BackendSSH:
		// Reached only for an ssh target that is NOT a listed host (#5571 W7.4): a
		// listed host carries the account through W7.3's synced scope, so the old
		// "refused by design" reasoning no longer applies to it. What remains is
		// that af will not send an account to a target an operator did not list.
		return listedHostAccountReason + "; " + listedHostAccountRemedy()

	case BackendSandbox:
		// The same round-trip reason as ssh, and NOT a "no provable location" one:
		// sandbox runs through sandboxProvisioner, which creates the session directory
		// itself, so af controls the location there too (#3103 review).
		//
		// And like hook, the workaround must NOT name an identity. sandbox.ssh is the
		// operator's own command and this backend deliberately preserves their
		// ssh_config — so a `SendEnv OPENAI_API_KEY` block copies the DAEMON's value
		// to the sandbox verbatim (measured in #3092), and the session would run on
		// the daemon host's identity while the operator believed otherwise. Only
		// backend=ssh is immune, because af pins -F none there.
		return offBoxRoundTripReason +
			" Use the docker or local backend for account-scoped sessions, or omit --account — though which " +
			"identity the session then runs as depends on your sandbox.ssh command and the ssh_config it " +
			"reads, which af does not override"

	case BackendHook:
		// Same reason, but the WORKAROUND must not promise an identity here.
		// runHookScriptWithResolvedEnvironment hands launch_cmd the DAEMON's filtered
		// agent-authentication environment and the script decides what reaches the
		// machine — so "omit --account to use that machine's own credentials" would be
		// false, and false in the exact direction account scoping exists to prevent:
		// the session could run as the daemon host's ambient identity while the
		// operator believes it is the remote's (#3103 review).
		return offBoxRoundTripReason +
			" Use the docker or local backend for account-scoped sessions, or omit --account — though which " +
			"identity the session then runs as is up to your hooks, since af passes the daemon's filtered " +
			"agent credentials to them"
	default:
		// A backend added later, refused by default. Naming it as unproven rather
		// than as impossible keeps this honest for something nobody has assessed.
		return "af has not established that it can carry a credential account onto that machine, and " +
			"running there would use its ambient credentials while reporting the account you asked for; " +
			"use the docker or local backend for account-scoped sessions, or omit --account"
	}
}

// refuseUnsupportedAccountAgent rejects a cross-agent override or an agent whose
// launch/account contract af has not established. It deliberately does NOT judge
// command shape here: program_overrides can be re-read before launch, and only
// prepareCreateLaunch has the frozen resolved command plus af's generated-argument
// declaration. That final admission names any undeclared arguments before the pane
// starts (#3108).
//
// This remains an ALLOWLIST, not a denylist: an agent added later is unsupported
// until someone proves the boundary accepts its launch, which fails in the safe
// direction (#3051, #3083).
func refuseUnsupportedAccountAgent(opts InstanceOptions, absPath string) error {
	if strings.TrimSpace(opts.Account) == "" {
		return nil
	}
	// RESOLVE FIRST, then decide (#3083 review, #3108).
	//
	// opts.Program is a LABEL, and program_overrides can point it at another agent
	// entirely. The account name was validated in the namespace of the REQUESTED
	// agent (api/sessions.go), but the launch derives the agent from the RESOLVED
	// command — so `Program=claude` + `program_overrides.claude=codex` validates
	// "work" as a claude account and then runs codex under CODEX's "work". Two
	// namespaces, one name, and the session silently authenticates as someone the
	// user never selected. That is the exact failure this feature exists to prevent,
	// and a gate reading the label cannot see it.
	//
	// config.ResolveProgram is the resolver the launch itself uses
	// (resolveProgramForAgent), so this gate and the launch cannot disagree about
	// what the command is.
	// The config is resolved HERE rather than threaded in, because the gate must read
	// the same overrides the launch will: resolveRepoConfig is what the launch path
	// uses too. A failure to resolve leaves cfg nil, and config.ResolveProgram on a
	// nil config returns the label unchanged — which is the pre-override answer, so an
	// unreadable config cannot silently admit a cross-agent override.
	// Repo config, then GLOBAL — mirroring resolveConfigForInstance exactly, because
	// that is what the launch uses. Trying only the repo would miss a global
	// program_overrides entry that the launch then applies, which is the very
	// gate-disagrees-with-launch divergence this check exists to close.
	var cfg *config.Config
	if resolved, rerr := resolveRepoConfig(absPath); rerr == nil {
		cfg = &resolved.Config
	} else if loaded, lerr := config.LoadConfig(); lerr == nil {
		cfg = loaded
	}
	requested := sessionenv.AgentForCommand(opts.Program)
	agent := sessionenv.AgentForCommand(config.ResolveProgram(cfg, opts.Program))
	if agent != requested {
		return accountRefusalWithSource(opts, fmt.Errorf(
			"account %q was validated as a %s account, but this session's program_overrides resolves %s to "+
				"a %s command — the account namespaces are separate, so a same-named %s account would be "+
				"used instead of the %s one you selected. Remove the override, or create the session on the "+
				"agent whose account you mean",
			opts.Account, requested, requested, agent, agent, requested))
	}
	// Still an EXPLICIT list, and still deliberately NOT sessionenv.SupportsAccounts:
	// that answers "does this agent have a credential-root variable", which is a
	// different question from "can af prove its launch to the boundary". Keeping them
	// separate is what makes an agent added to the roster unsupported HERE until
	// someone checks the second question, which fails in the safe direction (#3051,
	// #3083). claude is on the list because af's launch rewrite carries provenance the
	// boundary verifies — the launcher declares `--session-id`/`--plugin-dir`, so the
	// rewritten command is provable rather than refused; codex needs no declaration,
	// because af leaves its command unmodified.
	//
	// What changed in #3609 is only WHERE the list lives. This function kept its own
	// copy, so once gemini joined the roster `af accounts add gemini work` succeeded
	// and `--account work` answered "supported: claude, codex" — a sentence the roster
	// had just made false, and the user had no way to tell which surface was wrong.
	// sessionenv owns the one list now, and every surface says the same thing about a
	// rostered-but-unproven agent.
	//
	// gemini joined the list in #3639, by running the check rather than waiving it:
	// af hands its pane the resolved command unchanged, so ValidateAccountCommand
	// proves it without a declaration, and a scoped session was observed using the
	// account directory while an unscoped control kept the ambient identity. The two
	// lists agree again, which is the normal state — the registration-only surfaces
	// exist for the gap between them, not as a permanent fixture.
	if sessionenv.AccountLaunchProven(agent) {
		return nil
	}
	// A ROSTERED agent gets the registration-only sentence, because "unsupported" is
	// false for it: the account exists, the login works, and only the launch is
	// unproven. Naming the follow-up is what makes that a state rather than a dead end.
	if reason, ok := sessionenv.AccountRegistrationOnlyReason(agent); ok {
		return accountRefusalWithSource(opts,
			fmt.Errorf("account %q is registered for %s, but %s", opts.Account, agent, reason))
	}
	return accountRefusalWithSource(opts, fmt.Errorf(
		"account %q cannot be used with %s yet: af has not established that the account boundary can "+
			"verify how it launches that agent, so the session could start on the ambient identity or exit "+
			"immediately; account scoping supports %s",
		opts.Account, agent, sessionenv.AccountAgentsSummary()))
}

// accountRefusalWithSource appends where a non-requested account came from, so a
// refusal whose remedy is "omit --account" is not handed to someone with no
// --account to omit (#3386).
//
// The clause goes LAST, after the refusal's own reason, because that reason is
// what the user has to understand first. The separator is " · " and not a bare
// concatenation for a reason the registration-only arm makes concrete: that
// sentence can END IN A URL, and only whitespace terminates one for a terminal's
// link detector — a period or a letter fused to it becomes part of the address.
// AccountSource is itself a complete sentence, so this only joins the two.
func accountRefusalWithSource(opts InstanceOptions, err error) error {
	source := strings.TrimSpace(opts.AccountSource)
	if source == "" || err == nil {
		return err
	}
	return fmt.Errorf("%w · %s", err, source)
}

// resolveAccountForProvision resolves a create's --account to the registered
// account's directory on this host, in the REQUESTED program's namespace.
//
// A create names the account in the enum's registry — the API validated it
// there — so a program_overrides redirect that moves the enum to another
// agent's command must refuse rather than silently spend the resolved agent's
// same-named account (#4430 review, D2). The swap path deliberately differs:
// it selects under the command the session is RUNNING (the resolved one),
// because the session's live identity, not its recorded enum, is what the
// account will pin.
func resolveAccountForProvision(repoRoot, program, accountName string) (sessionenv.Account, error) {
	resolved, err := resolveRepoConfig(repoRoot)
	if err != nil {
		return sessionenv.Account{}, fmt.Errorf("cannot resolve account %q against the session program: %w", accountName, err)
	}
	resolvedProgram := config.ResolveProgram(&resolved.Config, program)
	// A create requests an account in the requested enum's namespace; when
	// program_overrides resolves the enum to another agent's command, silently
	// spending the RESOLVED agent's same-named account is the name-collision
	// hazard the local path refuses via refuseAccountAgentDrift — both backends
	// must give the same answer to the same command (#4430 review).
	requestedAgent := sessionenv.AgentForCommand(program)
	resolvedAgent := sessionenv.AgentForCommand(resolvedProgram)
	if resolvedAgent != requestedAgent {
		return sessionenv.Account{}, fmt.Errorf(
			"account %q is a %s account, but this session resolves %s to a %s command; account namespaces are separate, so the session would not use the identity you selected",
			accountName, requestedAgent, requestedAgent, resolvedAgent)
	}
	return selectAccountInNamespace(requestedAgent, accountName)
}

// selectAccountInNamespace resolves accountName inside agent's account
// registry. It is the shared tail of account selection: fresh provisions reach
// it through resolveAccountForProvision's refused-or-matched requested
// namespace, a swap reaches it with the resolved launch command's agent, and a
// committed manual swap's retry reaches it with the namespace the transaction
// recorded at commit — the one answer a later config flip cannot move (#4430
// review).
func selectAccountInNamespace(agent, accountName string) (sessionenv.Account, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return sessionenv.Account{}, fmt.Errorf("cannot resolve account %q: %w", accountName, err)
	}
	account, err := agentaccount.Selected(home, agent, accountName)
	if err != nil {
		return sessionenv.Account{}, err
	}
	if account.Dir == "" {
		// Selected returns a zero Account for an empty name, which the caller has
		// already excluded. A zero Dir here would mean an unscoped provision that
		// still reported an account, so refuse rather than proceed.
		return sessionenv.Account{}, fmt.Errorf("account %q resolved to no directory", accountName)
	}
	return account, nil
}
