package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// THE CONSTRAINT (#2983). A session must receive exactly ONE account's
// credential and have the others actively removed.
//
// This is written as an EXCLUSION test on purpose. The naive implementation —
// delivering the account through the session-env allowlist — passes every
// presence check: the credential is there, the agent authenticates, the flag was
// accepted. What it does not do is remove the ambient identity, and for both of
// these CLIs an ambient API key takes precedence over the config directory. So
// "the session has a credential" is exactly the assertion that cannot tell the
// working implementation from the broken one.
func TestApplyAccount_RemovesTheAmbientIdentity(t *testing.T) {
	ambient := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ambient-should-not-survive",
		"ANTHROPIC_AUTH_TOKEN=tok-ambient-should-not-survive",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-ambient-should-not-survive",
		"CLAUDE_CONFIG_DIR=/home/op/.claude",
		"ANTHROPIC_BASE_URL=https://proxy.internal",
	}

	scoped, err := ApplyAccount(ambient, "", Account{Agent: "claude", Name: "work", Dir: "/afhome/accounts/claude/work"})
	require.NoError(t, err)

	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		_, present := envValue(scoped, name)
		require.False(t, present,
			"%s survived into an account-scoped session: an ambient key outranks the config directory, so the account selection would be silently ignored", name)
	}

	dir, ok := envValue(scoped, "CLAUDE_CONFIG_DIR")
	require.True(t, ok, "the account's credential root must be injected")
	require.Equal(t, "/afhome/accounts/claude/work", dir,
		"the session must get the SELECTED account's directory, not the daemon's ambient one")

	// The operator's deployment config is not an identity and must survive, or
	// scoping an account would break a proxied install.
	base, ok := envValue(scoped, "ANTHROPIC_BASE_URL")
	require.True(t, ok)
	require.Equal(t, "https://proxy.internal", base)
	path, ok := envValue(scoped, "PATH")
	require.True(t, ok)
	require.Equal(t, "/usr/bin", path)
}

// The ambient config dir must be REPLACED, not merely joined. If both survived,
// which one wins is the agent's parsing order rather than af's decision.
func TestApplyAccount_ReplacesRatherThanAppendsTheConfigDir(t *testing.T) {
	scoped, err := ApplyAccount(
		[]string{"CODEX_HOME=/home/op/.codex"}, "",
		Account{Agent: "codex", Name: "personal", Dir: "/afhome/accounts/codex/personal"},
	)
	require.NoError(t, err)

	count := 0
	for _, kv := range scoped {
		if strings.HasPrefix(kv, "CODEX_HOME=") {
			count++
		}
	}
	require.Equal(t, 1, count, "exactly one CODEX_HOME must reach the session")
	dir, _ := envValue(scoped, "CODEX_HOME")
	require.Equal(t, "/afhome/accounts/codex/personal", dir)
}

func TestApplyAccountEnvironment_ScopesSiblingAndRemovesAmbientIdentity(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	scoped, err := ApplyAccountEnvironment([]string{
		"PATH=/usr/bin",
		"CODEX_HOME=/home/op/.codex",
		"OPENAI_API_KEY=sk-ambient",
		"BASH_ENV=/tmp/ambient-bashrc",
		"PS1=$((CODEX_HOME=42))",
	}, "make -j4", account)
	require.NoError(t, err)
	require.Contains(t, scoped, "CODEX_HOME="+account.Dir)
	_, leaked := envValue(scoped, "OPENAI_API_KEY")
	require.False(t, leaked, "a process sibling must not inherit the ambient API identity")
	_, startupHook := envValue(scoped, "BASH_ENV")
	require.False(t, startupHook, "the outer /bin/sh -c must not source an admitted startup hook")
	_, executablePrompt := envValue(scoped, "PS1")
	require.False(t, executablePrompt, "a later interactive shell must not inherit executable prompt text")
}

func TestApplyAccountEnvironment_RefusesDirectIdentityOverride(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"OPENAI_API_KEY=other make",
		"OPENAI_API_KEY=other make >build.log",
		"OPENAI_API_KEY=other make &",
		"unset CODEX_HOME; make",
		"command unset CODEX_HOME; make",
		"export OPENAI_API_KEY=other; make",
		"readonly OPENAI_API_KEY=other; make",
		"read CODEX_HOME </dev/null; make",
		"printf -v CODEX_HOME other; make",
		"eval 'unset CODEX_HOME'; make",
		"alias run='OPENAI_API_KEY=other codex'; run",
		"exec -c env HOME=$HOME codex",
		"sh -c 'OPENAI_API_KEY=other codex'",
		"/bin/bash -lc 'unset CODEX_HOME; codex'",
		"env sh -c 'OPENAI_API_KEY=other codex'",
		"sh <<'EOF'\nunset CODEX_HOME\ncodex\nEOF",
		"bash -i",
		"PROMPT_COMMAND='export CODEX_HOME=/other' /bin/bash --noprofile --norc -i",
		"ENV=/tmp/ambient-shrc /bin/sh -i",
		"env PROMPT_COMMAND='export CODEX_HOME=/other' /bin/bash --noprofile --norc -i",
		"env PROMPT_COMMAND='export CODEX_HOME=/other' nohup /bin/bash --noprofile --norc -i",
		"export PROMPT_COMMAND='export CODEX_HOME=/other'; /bin/bash --noprofile --norc -i",
		"BASH_ENV=/tmp/ambient-bashrc make",
		"nohup sh -c 'unset CODEX_HOME; codex'",
		"/usr/bin/nohup sh -c 'unset CODEX_HOME; codex'",
		"env nohup sh -c 'unset CODEX_HOME; codex'",
		"nohup env sh -c 'unset CODEX_HOME; codex'",
		"nice sh -c 'unset CODEX_HOME; codex'",
		"/usr/bin/nice sh -c 'unset CODEX_HOME; codex'",
		"typeset -n ref=CODEX_HOME; ref=/other; codex",
		"unset 'CODEX_HOME[0]'; codex",
		"export 'CODEX_HOME[0]=/other'; codex",
		"let CODEX_HOME=42; codex",
		"let 'CODEX_HOME[0]=42'; codex",
		"mapfile CODEX_HOME </dev/null; codex",
		"mapfile -t CODEX_HOME </dev/null; codex",
		"mapfile -O 'CODEX_HOME=42' DATA </dev/null; codex",
		"readarray CODEX_HOME </dev/null; codex",
		"for CODEX_HOME in /other; do make; done",
		"op=unset; $op CODEX_HOME; make",
		"env CODEX_HOME=/other make",
		"/usr/bin/env CODEX_HOME=/other make",
		"env CODEX_HOME=$HOME/.codex make",
		"env -S 'CODEX_HOME=/other make'",
		"command env CODEX_HOME=/other make",
		"env -uCODEX_HOME make",
		"env -i make",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "command %q must not replace the sibling account environment", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
	}
	_, err := ApplyAccountEnvironment(nil, "CLAUDE_CODE_USE_BEDROCK=$MODE make",
		Account{Agent: "claude", Name: "work", Dir: "/afhome/accounts/claude/work"})
	require.Error(t, err, "a dynamic provider selector can redirect a sibling away from the selected account")
	require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
}

func TestApplyAccountEnvironment_RefusesTimeoutWrappedShell(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"timeout 10 sh -c 'unset CODEX_HOME; codex'",
		"/usr/bin/timeout 10 sh -c 'unset CODEX_HOME; codex'",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "timeout must not hide a nested identity mutation in %q", command)
	}
}

func TestApplyAccountEnvironment_RefusesSetsidWrappedShell(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"setsid sh -c 'unset CODEX_HOME; codex'",
		"/usr/bin/setsid sh -c 'unset CODEX_HOME; codex'",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "setsid must not hide a nested identity mutation in %q", command)
	}
}

func TestApplyAccountEnvironment_RefusesStdbufWrappedShell(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"stdbuf -oL sh -c 'unset CODEX_HOME; codex'",
		"/usr/bin/stdbuf -oL sh -c 'unset CODEX_HOME; codex'",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "stdbuf must not hide a nested identity mutation in %q", command)
	}
}

func TestApplyAccountEnvironment_RefusesBashArithmeticCommand(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	_, err := ApplyAccountEnvironment(nil, "(( 0, CODEX_HOME=42 )); codex", account)
	require.Error(t, err, "Bash arithmetic-command syntax must not bypass identity mutation checks")
}

func TestApplyAccountEnvironment_RefusesArraySubscriptSideEffect(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	_, err := ApplyAccountEnvironment(nil, "unset 'arr[CODEX_HOME=42]'; codex", account)
	require.Error(t, err, "array-subscript arithmetic must not mutate identity through an unrelated base name")
}

func TestApplyAccountEnvironment_RefusesWaitResultIdentity(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"sleep 0 & wait -p CODEX_HOME $!; codex",
		"sleep 0 & wait -p RESULT -p CODEX_HOME -n; codex",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "wait -p must not replace the selected identity variable in %q", command)
	}
}

func TestApplyAccountEnvironment_RefusesHistoryReplay(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	_, err := ApplyAccountEnvironment(nil, "set -o history; history -r ./commands; fc -s; codex", account)
	require.Error(t, err, "history replay must not execute an unvalidated identity mutation")
}

func TestApplyAccountEnvironment_RefusesIntegerDeclarationSideEffect(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	_, err := ApplyAccountEnvironment(nil, "declare -i x=CODEX_HOME=42; codex", account)
	require.Error(t, err, "integer-attributed declarations must not evaluate an identity assignment")
}

func TestApplyAccountEnvironment_RefusesDynamicBuiltinLoading(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	_, err := ApplyAccountEnvironment(nil, "enable -f ./evil.so evil; evil; codex", account)
	require.Error(t, err, "a dynamically loaded builtin can mutate the current shell environment")
}

func TestApplyAccountEnvironment_RefusesArrayVariableTests(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"test -v 'arr[CODEX_HOME=42]'; codex",
		"[ -v 'arr[CODEX_HOME=42]' ]; codex",
		"[[ -v 'arr[CODEX_HOME=42]' ]]; codex",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "array variable test must not evaluate an identity mutation in %q", command)
	}
}

func TestApplyAccountEnvironment_AllowsNonIdentityAssignments(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"PORT=3000 npm start",
		"PORT=3000 npm start >server.log",
		"PORT=3000 npm start &",
		"unset PORT; make",
		"command unset PORT; make",
		"export PORT=3000; make",
		"readonly PORT=3000; make",
		"let PORT=42; make",
		"mapfile DATA </dev/null; make",
		"readarray -t DATA </dev/null; make",
		"sleep 0 & wait -p PID $!; make",
		"nohup make",
		"nice -n 5 make",
		"timeout 10 make",
		"setsid make",
		"stdbuf -oL make",
		"for PORT in 3000; do make; done",
		"NODE_ENV=test make",
		"env PORT=3000 npm start",
		"command env PORT=3000 npm start",
	} {
		scoped, err := ApplyAccountEnvironment(nil, command, account)
		require.NoError(t, err, "command %q only sets process configuration", command)
		require.Contains(t, scoped, "CODEX_HOME="+account.Dir)
	}
}

func TestAccountShellCommandDisablesStartupFiles(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for shell, want := range map[string]string{
		"/bin/bash": "/bin/bash --noprofile --norc -i",
		"/bin/csh":  "/bin/csh -f -i",
	} {
		command, err := AccountShellCommand(shell)
		require.NoError(t, err)
		require.Equal(t, want, command)

		scoped, err := ApplyAccountEnvironment([]string{
			"PATH=/bin",
			"ENV=/tmp/ambient-shrc",
			"BASH_ENV=/tmp/ambient-bashrc",
			"ZDOTDIR=/tmp/ambient-zdotdir",
			"PROMPT_COMMAND=export CODEX_HOME=/tmp/ambient-codex",
			"PS1=$((CODEX_HOME=42))",
		}, command, account)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"PATH=/bin", "CODEX_HOME=" + account.Dir}, scoped)
	}
}

func TestAccountShellCommandRefusesShellsWithoutCredentialSafeStartup(t *testing.T) {
	for _, shell := range []string{"/bin/fish", "/bin/zsh", "/opt/company/bash"} {
		_, err := AccountShellCommand(shell)
		require.Error(t, err, "%s can restore identity variables after the account environment is installed", shell)
		require.Contains(t, err.Error(), "no credential-safe account launch mode")
	}
}

// Two sessions on different accounts must get different roots. This is the
// assertion an allowlist implementation fails: passthrough carries the daemon's
// ONE value to every session, so both would be identical while each looked
// correct in isolation.
func TestApplyAccount_DifferentAccountsGetDifferentRoots(t *testing.T) {
	ambient := []string{"CLAUDE_CONFIG_DIR=/home/op/.claude"}

	a, err := ApplyAccount(ambient, "", Account{Agent: "claude", Name: "a", Dir: "/afhome/accounts/claude/a"})
	require.NoError(t, err)
	b, err := ApplyAccount(ambient, "", Account{Agent: "claude", Name: "b", Dir: "/afhome/accounts/claude/b"})
	require.NoError(t, err)

	dirA, _ := envValue(a, "CLAUDE_CONFIG_DIR")
	dirB, _ := envValue(b, "CLAUDE_CONFIG_DIR")
	require.NotEqual(t, dirA, dirB, "two accounts must not resolve to one credential root")
	require.Equal(t, "/afhome/accounts/claude/a", dirA)
	require.Equal(t, "/afhome/accounts/claude/b", dirB)
}

// An agent whose credential relocation was never VERIFIED must refuse, not
// silently accept a selection that does nothing. Allowlist membership is not
// evidence: amp and opencode both HAVE agent-specific variables that relocate
// only settings/config, so an entry taken from the allowlist would have scoped
// nothing while looking like it worked (#3387 measured both).
//
// gemini left this list in #3387 because its relocation was actually verified —
// see accountConfigVars — which is the only way an agent may leave it.
func TestApplyAccount_RefusesAnUnverifiedAgent(t *testing.T) {
	for _, agent := range []string{"amp", "aider", "opencode", "devin", "unknown"} {
		_, err := ApplyAccount(nil, "", Account{Agent: agent, Name: "x", Dir: "/d"})
		require.Error(t, err, "agent %q must refuse rather than accept an inert account selection", agent)
		require.Contains(t, err.Error(), "does not support multiple accounts")
		require.Contains(t, err.Error(), "claude", "the error must name what IS supported")
	}
}

// An account with no directory is a configuration error, never an empty scope
// that silently falls back to the ambient identity.
func TestApplyAccount_RefusesAnEmptyDirectory(t *testing.T) {
	_, err := ApplyAccount(nil, "", Account{Agent: "claude", Name: "work", Dir: "   "})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no credential directory")
}

// Every allowlisted name for a scoped agent must be classified as either an
// identity (removed) or deliberately kept. Without this the subtraction list
// rots silently: a credential added to agentNames later would keep passing
// through, and the exclusion tests above would still be green because they only
// name the variables that existed when they were written.
func TestAccountCredentialsAreClassified(t *testing.T) {
	for agent := range accountConfigVars {
		for name := range agentNames[agent] {
			_, isCredential := accountCredentialNames[agent][name]
			_, isKept := accountNonCredentialNames[agent][name]
			require.True(t, isCredential || isKept,
				"%s is allowlisted for %s but classified neither as an identity to remove nor as deployment config to keep; "+
					"an unclassified credential passes through and silently overrides the selected account", name, agent)
			require.False(t, isCredential && isKept,
				"%s for %s is classified both ways", name, agent)
		}
	}
}

// The config variable itself is always removed before injection, so an ambient
// copy cannot shadow the account.
func TestAccountScopedNames_AlwaysDeniesTheConfigVar(t *testing.T) {
	for agent, configVar := range accountConfigVars {
		denied := accountScopedNames(agent, configVar)
		_, ok := denied[configVar]
		require.True(t, ok, "%s must be denied before it is injected for %s", configVar, agent)
	}
}

// A CLOUD MODE authenticates somewhere else entirely, so an account cannot scope
// it. Bedrock/Vertex/Foundry make the CLI use AWS/Google/Azure credentials —
// which FilterForCommand deliberately admits for exactly those modes — so the
// account directory stops being the session's identity while still looking like
// it. Refusing beats removing the selector, which would silently move the
// session off the deployment mode its operator configured.
func TestApplyAccount_RefusesWhileACloudModeIsActive(t *testing.T) {
	for _, selector := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		env := []string{selector + "=1", "AWS_SECRET_ACCESS_KEY=ambient"}
		_, err := ApplyAccount(env, "", Account{Agent: "claude", Name: "work", Dir: "/afhome/accounts/claude/work"})
		require.Error(t, err, "%s active must refuse an account scope", selector)
		require.Contains(t, err.Error(), "cloud mode")
		require.Contains(t, err.Error(), selector, "the refusal must name which mode blocked it")
	}

	// With no selector set, the same agent scopes normally — so the guard cannot
	// pass by refusing everything.
	_, err := ApplyAccount([]string{"PATH=/usr/bin"}, "",
		Account{Agent: "claude", Name: "work", Dir: "/afhome/accounts/claude/work"})
	require.NoError(t, err)
}

// A COMMAND-LOCAL ASSIGNMENT wins over the injected root, because the launch runs
// the program through `/bin/sh -c` and the shell applies it after this
// environment is installed. program_overrides is reachable from a repository's
// checked-in config, so this is a repo-controlled way to redirect whose quota a
// session spends — the exact thing account scoping exists to control.
func TestApplyAccount_RefusesACommandThatSetsTheConfigVar(t *testing.T) {
	cases := []string{
		"CODEX_HOME=/other codex",
		// Forms the first, denylist-shaped version walked straight past.
		"env --unset=CODEX_HOME codex",
		"env -uCODEX_HOME codex",
		"env - codex",
		// Sets the variable AND wraps a shell: the assignment is the stronger,
		// definite answer, so this is an override rather than merely unprovable.
		"env CODEX_HOME=/other sh -c codex",
		// Any identity-bearing name, not just the config root: subtraction removed
		// the ambient copy, and the shell recreates it afterwards.
		"OPENAI_API_KEY=sk-other codex",
		"CODEX_API_KEY=other codex",
		"env OPENAI_API_KEY=sk-other codex",
		// A cloud selector whose value the parser cannot evaluate reads as
		// DISABLED, so the cloud-mode refusal never fires while the shell expands
		// it to a non-empty string.
		"CLAUDE_CODE_USE_BEDROCK=$HOME codex",
		// Any assignment at all, including ones that carry no identity but change
		// what the executable DOES.
		"LD_PRELOAD=./steal.so codex",
		"DYLD_INSERT_LIBRARIES=./steal.dylib codex",
		// The env wrapper must express the same rule as a shell assignment: a
		// scoped program mutates nothing. PATH is the sharpest of these — it
		// redirects the bare executable the provenance rule depends on.
		"env LD_PRELOAD=./steal.so codex",
		"env PATH=. codex",
		// A chdir is a PATH mutation in disguise: it changes command lookup.
		"env -C attacker codex",
		"env --chdir=attacker codex",
		"CODEX_HOME=$HOME/.codex codex",
		"env CODEX_HOME=/other codex",
		"env -u CODEX_HOME codex",
		"env -i codex",
		"exec CODEX_HOME=/other codex",
	}
	for _, command := range cases {
		_, err := ApplyAccount(nil, command, Account{Agent: "codex", Name: "p", Dir: "/afhome/accounts/codex/p"})
		require.Error(t, err, "command %q must not silently override the account root", command)
		require.Contains(t, err.Error(), "identity variable")
	}

	// An ordinary program is unaffected, so the guard is not simply refusing all
	// account use.
	_, err := ApplyAccount(nil, "codex", Account{Agent: "codex", Name: "p", Dir: "/afhome/accounts/codex/p"})
	require.NoError(t, err)
}

// A command that cannot be parsed into a single simple call is not evidence of
// safety. Account scoping decides whose quota is spent, so an unprovable program
// fails closed rather than being assumed harmless.
func TestApplyAccount_FailsClosedOnAnUnprovableCommand(t *testing.T) {
	for _, command := range []string{
		"codex | tee /tmp/x",
		"(CODEX_HOME=/other codex)",
		"codex && echo done",
		// A shell wrapper parses as an ordinary single call whose ARGUMENT is
		// another whole program. It must be unprovable, not "provably fine".
		"sh -c 'CODEX_HOME=/other codex'",
		"bash -lc codex",
		// Any binary whose behaviour is not modelled here.
		"/usr/local/bin/wrapper codex",
		// A command substitution in an ARGUMENT runs before the outer agent does.
		"codex \"$(unset CODEX_HOME; codex exec x)\"",
		"codex --model $(cat /tmp/m)",
		// A basename is not provenance: ./codex is an arbitrary repo-provided file
		// that would receive the selected root.
		"./codex",
		"bin/codex",
		// A repository file that merely SHARES a name with a modelled wrapper.
		"./env codex",
		"./af agent-server --program codex --program-resolved",
		// An agent's own flags redirect its identity as effectively as the
		// environment: codex -c cli_auth_credentials_store="keyring" ignores the
		// account directory's auth.json entirely.
		"codex -c cli_auth_credentials_store=\"keyring\"",
		"codex --model gpt-5",
		// The no-argument rule must hold through the env wrapper too.
		"env codex -c cli_auth_credentials_store=keyring",
		"env codex --model gpt-5",
		// Case-sensitive hosts: CODEX is a different executable from codex.
		"CODEX",
		"env CODEX",
		// An ignored signal disposition SURVIVES exec: the agent could outlive
		// teardown while holding the account.
		"env --ignore-signal=HUP,TERM codex",
		"env --block-signal=TERM codex",
		"env --default-signal codex",
		"ENV codex",
		// A CROSS-AGENT override: the account scopes codex, the command runs
		// claude, which ignores CODEX_HOME entirely and uses its own default home.
		"claude",
		// One executable operand whose NAME contains a space. Re-parsing it as a
		// command string would split it and see "./codex"; the shell runs a
		// repository-provided file called "codex wrapper".
		"env './codex wrapper'",
	} {
		_, err := ApplyAccount(nil, command, Account{Agent: "codex", Name: "p", Dir: "/afhome/accounts/codex/p"})
		require.Error(t, err, "command %q is unparseable and must not be assumed safe", command)
		require.Contains(t, err.Error(), "could not be proven")
	}
}

// The docker and ssh backends generate an ABSOLUTE af path for the agent-server
// handoff (`/usr/local/bin/af agent-server …`, or a staged path). A bare-name
// rule refuses af's own launch on those backends, so account scoping would fail
// for every session there — the name-is-not-provenance problem in reverse: the
// path IS trusted and its spelling cannot say so, which is why the launcher
// supplies it rather than this parsing it out (#2983).
func TestApplyAccount_AcceptsTheLaunchersOwnAfWrapper(t *testing.T) {
	const generated = "/usr/local/bin/af"
	command := generated + " agent-server --listen x --repo r --title t --program codex --program-resolved"

	// Without the supplied provenance the same command is unprovable: an absolute
	// path proves nothing on its own.
	_, err := ApplyAccount(nil, command, Account{Agent: "codex", Name: "p", Dir: "/afhome/accounts/codex/p"})
	require.Error(t, err, "an absolute af path with no supplied provenance must stay unprovable")

	scoped, err := ApplyAccount(nil, command, Account{
		Agent: "codex", Name: "p", Dir: "/afhome/accounts/codex/p", TrustedWrapper: generated,
	})
	require.NoError(t, err, "the launcher's own handoff must be accepted, or account scoping fails on docker and ssh")
	dir, ok := envValue(scoped, "CODEX_HOME")
	require.True(t, ok)
	require.Equal(t, "/afhome/accounts/codex/p", dir)
}

// Provenance is an EXACT path, never a basename. A repository file sharing the
// name — or sitting at a different path — is still refused even when a trusted
// wrapper was supplied.
func TestApplyAccount_TrustedWrapperIsNotABasename(t *testing.T) {
	for _, executable := range []string{"./af", "/repo/af", "/usr/local/bin/af-shim"} {
		command := executable + " agent-server --program codex --program-resolved"
		_, err := ApplyAccount(nil, command, Account{
			Agent: "codex", Name: "p", Dir: "/d", TrustedWrapper: "/usr/local/bin/af",
		})
		require.Error(t, err, "%s is not the generated wrapper and must not inherit its trust", executable)
	}
}

// TestApplyAccount_ScopesGemini pins the roster entry #3387 added on evidence.
//
// The subtraction half is the point: gemini reads GEMINI_API_KEY and
// GOOGLE_API_KEY as identities that outrank the config directory, so a session
// that selected an account while either passed through would authenticate as
// whoever that key belongs to while every visible signal said otherwise.
func TestApplyAccount_ScopesGemini(t *testing.T) {
	ambient := []string{
		"GEMINI_API_KEY=ambient-key",
		"GOOGLE_API_KEY=ambient-google-key",
		"GEMINI_CLI_HOME=/somewhere/else",
		"PATH=/usr/bin",
	}
	out, err := ApplyAccount(ambient, "", Account{
		Agent: "gemini", Name: "work", Dir: "/afhome/accounts/gemini/work",
	})
	require.NoError(t, err)

	dir, ok := envValue(out, "GEMINI_CLI_HOME")
	require.True(t, ok, "the account must be injected through the verified variable")
	require.Equal(t, "/afhome/accounts/gemini/work", dir,
		"GEMINI_CLI_HOME is a HOME-like root: the CLI appends .gemini/ itself, so af "+
			"points it at the account directory rather than at a .gemini path")

	for _, name := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		value, present := envValue(out, name)
		require.True(t, present,
			"%s must survive as an EMPTY definition rather than be removed: gemini reads a repository "+
				"`.env` after this boundary and assigns only names it does not already find", name)
		require.Empty(t, value,
			"%s outranks the config directory, so leaving it with a value makes the account selection a lie", name)
	}
	_, keptPath := envValue(out, "PATH")
	require.True(t, keptPath, "scoping must not strip unrelated environment")

	require.Equal(t, 1, strings.Count(strings.Join(out, "\n")+"\n", "GEMINI_API_KEY="),
		"the ambient value must be REPLACED, not shadowed by a later entry: a consumer that keeps the "+
			"first occurrence would read the ambient key back")
}

// TestApplyAccount_PinsGeminiIdentityNamesAgainstAProjectDotenv is the half that
// subtraction alone does not hold (#3609 review, Codex P1).
//
// Removing a name is enough only while nothing else writes to the environment
// between this boundary and the agent. gemini writes to it itself: it walks up
// from the workspace looking for `<dir>/.gemini/.env` and `<dir>/.env` at every
// level and applies what it finds, so a checked-in `.env` recreates exactly the
// names removed here and the session authenticates as the repository while every
// visible signal reports the selected account.
//
// Measured against gemini 0.51.0 under a throwaway HOME. With the names absent
// and a repository `.env` naming them, the CLI announced "Both GOOGLE_API_KEY and
// GEMINI_API_KEY are set. Using GOOGLE_API_KEY." and sent a real request that came
// back `400 API key not valid`. With the names present and EMPTY, the identical
// run reported no auth method at all — the same output as a directory with no
// `.env`. A `.env` naming only GOOGLE_APPLICATION_CREDENTIALS signed a JWT with
// that service-account key against an oauth-personal account, and one naming
// GOOGLE_GENAI_USE_VERTEXAI moved the session onto Vertex; both were held off by
// the same empty definition.
func TestApplyAccount_PinsGeminiIdentityNamesAgainstAProjectDotenv(t *testing.T) {
	out, err := ApplyAccount([]string{"PATH=/usr/bin"}, "", Account{
		Agent: "gemini", Name: "work", Dir: "/afhome/accounts/gemini/work",
	})
	require.NoError(t, err)

	for _, name := range []string{
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA",
	} {
		value, present := envValue(out, name)
		require.True(t, present,
			"%s must be DEFINED so gemini's own `.env` loader will not assign it; removing it leaves the "+
				"name free for a repository file to claim", name)
		require.Empty(t, value, "%s must be defined EMPTY, never carry a value", name)
	}

	// The config variable is injected with the account, never pinned empty — an
	// empty copy of it would erase the selection this whole function makes.
	dir, ok := envValue(out, "GEMINI_CLI_HOME")
	require.True(t, ok)
	require.Equal(t, "/afhome/accounts/gemini/work", dir)

	// codex has no measured late environment source, so its boundary is unchanged:
	// names are removed, not pinned. Pinning an unmeasured agent would be a guess
	// about how its CLI reads an empty value.
	codex, err := ApplyAccount([]string{"OPENAI_API_KEY=ambient", "PATH=/usr/bin"}, "", Account{
		Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work",
	})
	require.NoError(t, err)
	_, present := envValue(codex, "OPENAI_API_KEY")
	require.False(t, present, "codex keeps the removal boundary it was measured with")
}

// Every identity a late-environment agent has must be pinned, not just the ones
// somebody remembered. A credential or selector added to that agent later and
// left out of the pinned set is a silent reopening of the `.env` hole, and it
// would pass every other test in this file.
func TestAccountLateEnvironmentNames_CoverEveryIdentityForThatAgent(t *testing.T) {
	for agent, pinned := range accountLateEnvironmentNames {
		configVar, ok := SupportsAccounts(agent)
		require.True(t, ok, "%s pins names but is not on the account roster", agent)
		_, pinsConfigVar := pinned[configVar]
		require.False(t, pinsConfigVar,
			"%s must be injected with the account directory, so pinning it empty would erase the selection", configVar)

		for name := range accountCredentialNames[agent] {
			_, ok := pinned[name]
			require.True(t, ok,
				"%s is an identity for %s but is only REMOVED, so the agent's own `.env` can put it back", name, agent)
		}
		for _, selector := range AgentAuthSelectors(agent) {
			_, ok := pinned[selector]
			require.True(t, ok,
				"%s selects a cloud identity for %s and is only removed, so a repository `.env` can turn "+
					"that mode on after this boundary has run", selector, agent)
		}
	}
}

// A provisioner must neutralize exactly what the local shim does. AccountIdentityNames
// is the shared classification, so a name the shim pins but the list omits is a
// docker or restored-tmux session with a hole the local one does not have.
func TestAccountIdentityNames_IncludeTheNamesTheShimPins(t *testing.T) {
	for agent, pinned := range accountLateEnvironmentNames {
		names := AccountIdentityNames(agent)
		for name := range pinned {
			require.Contains(t, names, name,
				"%s is pinned locally for %s but absent from the shared identity list, so a container "+
					"or restore path would leave it free", name, agent)
		}
	}
}

// TestApplyAccount_RefusesGeminiWhileACloudModeIsActive: gemini's two selectors
// are guarded exactly as Claude's Bedrock/Vertex/Foundry are (#2462), so adding
// the agent to the roster inherits the refusal rather than needing a new one.
// Vertex/GCA authenticate through Google Cloud credentials, so the account
// directory would stop being the session's identity while still appearing to be.
func TestApplyAccount_RefusesGeminiWhileACloudModeIsActive(t *testing.T) {
	for _, selector := range []string{"GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA"} {
		_, err := ApplyAccount([]string{selector + "=1"}, "",
			Account{Agent: "gemini", Name: "work", Dir: "/afhome/accounts/gemini/work"})
		require.Error(t, err, "%s active must refuse an account scope", selector)
		require.Contains(t, err.Error(), "cloud mode")
		require.Contains(t, err.Error(), selector, "the refusal must name which mode blocked it")
	}

	// Without a selector the same agent scopes normally, so the guard cannot
	// pass by refusing everything.
	_, err := ApplyAccount([]string{"PATH=/usr/bin"}, "",
		Account{Agent: "gemini", Name: "work", Dir: "/afhome/accounts/gemini/work"})
	require.NoError(t, err)
}

// The roster and the launch proof answer DIFFERENT questions, and #3609 is the
// first time an agent sat between them: gemini's credential root relocates, so it
// is on the roster, but nobody has verified how af launches it, so `--account`
// refuses. Two separate lists is the design — it is what makes a newly rostered
// agent fail closed at the launch boundary (#3051, #3083). What the two must not
// do is describe that state differently to the user.
func TestAccountRoster_SeparatesRegistrationFromLaunchProof(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		require.True(t, AccountLaunchProven(agent), "%s launches under an account today", agent)
		require.False(t, AccountRegistrationOnly(agent), "%s is not registration-only", agent)
		reason, ok := AccountRegistrationOnlyReason(agent)
		require.False(t, ok, "%s must yield no notice, so a caller cannot print one by forgetting to check", agent)
		require.Empty(t, reason)
	}

	_, supported := SupportsAccounts("gemini")
	require.True(t, supported, "gemini's credential root was measured to relocate (#3387)")
	require.False(t, AccountLaunchProven("gemini"), "gemini's launch proof is the open follow-up")
	require.True(t, AccountRegistrationOnly("gemini"))

	reason, ok := AccountRegistrationOnlyReason("gemini")
	require.True(t, ok)
	require.Contains(t, reason, "gemini", "the sentence must name which agent it is about")
	require.Contains(t, reason, accountLaunchProofIssueURL,
		"a state the operator cannot act on has to name the follow-up that lifts it")
	require.NotContains(t, reason, "does not support",
		"registration works, so calling it unsupported is the false half of the contradiction")

	// Every launch-proven agent is on the roster. The reverse is the interesting
	// direction and is deliberately allowed; this one is incoherent — a launch
	// proof for an agent with no credential root scopes nothing.
	for agent := range accountLaunchProvenAgents {
		_, ok := SupportsAccounts(agent)
		require.True(t, ok, "%s is launch-proven but has no credential root to launch into", agent)
	}
}

// Every "supported: …" list a user reads comes from AccountAgentsSummary, so a
// registration-only agent cannot appear in one as though a session could use it.
func TestAccountAgentsSummary_MarksRegistrationOnlyAgents(t *testing.T) {
	summary := AccountAgentsSummary()
	require.Contains(t, summary, "gemini ("+AccountRegistrationOnlyMarker+")")
	require.NotContains(t, summary, "claude ("+AccountRegistrationOnlyMarker+")")
	require.NotContains(t, summary, "codex ("+AccountRegistrationOnlyMarker+")")

	// AccountAgents stays BARE, because its callers index directories by these
	// names; a marked name would look up an account root that does not exist.
	require.Equal(t, []string{"claude", "codex", "gemini"}, AccountAgents())

	// And the refusal an unsupported agent gets carries the marked form.
	_, err := ApplyAccount(nil, "", Account{Agent: "aider", Name: "work", Dir: "/d"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "gemini ("+AccountRegistrationOnlyMarker+")")
}
