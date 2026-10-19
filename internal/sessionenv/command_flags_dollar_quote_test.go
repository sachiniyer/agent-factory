package sessionenv

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// commandEnvironmentFlagStateDollarQuoteCases is the canonical set of
// selector-assignment values that depend on a shell dialect for their
// expansion. The selector parser used to parse these under
// syntax.LangPOSIX alone, and the POSIX lexer does not recognize the
// $'…' (ANSI-C) / $"…" (locale) quote forms: it tokenizes them as a
// Lit{"$"} adjacent to an unmarked quote, so the value literalized to a
// "$"-prefixed string flagValueEnabled can never classify as falsy. The
// /bin/sh -c that actually runs the command DOES recognize them on
// bash-as-sh hosts (macOS, RHEL/Fedora, Arch), where it expands $'0' to
// 0 (falsy) — so the running process had the cloud mode OFF while
// FilterForCommand admitted the provider's whole credential group.
//
// Every value here MUST fail closed: it must neither enable a selector
// for ResolveAuthSelectors nor be reported as enabling cloud credentials
// by CommandEnablesCloudCredentials, regardless of the cloud provider
// (AWS / Google / Azure) or whether the assignment is bare or wrapped
// in env.
var commandEnvironmentFlagStateDollarQuoteCases = []string{
	// ANSI-C single-quote forms.
	`CLAUDE_CODE_USE_BEDROCK=$'0' claude`,
	`CLAUDE_CODE_USE_BEDROCK=$'false' claude`,
	`CLAUDE_CODE_USE_BEDROCK=$'no' claude`,
	`CLAUDE_CODE_USE_BEDROCK=$'off' claude`,
	`CLAUDE_CODE_USE_BEDROCK=$'' claude`,
	`CLAUDE_CODE_USE_BEDROCK=$'1' claude`,
	`CLAUDE_CODE_USE_BEDROCK=$'true' claude`,
	// Locale-translated double-quote forms.
	`CLAUDE_CODE_USE_BEDROCK=$"0" claude`,
	`CLAUDE_CODE_USE_BEDROCK=$"false" claude`,
	`CLAUDE_CODE_USE_BEDROCK=$"1" claude`,
	// Concatenated dollar quotes and dollar quotes mixed with ordinary
	// quoted text: still unprovable because the bash and POSIX parses
	// disagree on whether the word is dollar-quoted.
	`CLAUDE_CODE_USE_BEDROCK=$'0'$'0' claude`,
	`CLAUDE_CODE_USE_BEDROCK='0'$'0' claude`,
	`CLAUDE_CODE_USE_BEDROCK=a$'0' claude`,
	`CLAUDE_CODE_USE_BEDROCK=$'0'a claude`,
	// Any cloud-mode selector on any agent has the same parser path, and
	// every one of them must fail closed on a dialect-dependent value.
	`CLAUDE_CODE_USE_VERTEX=$'0' claude`,
	`CLAUDE_CODE_USE_FOUNDRY=$'0' claude`,
	`GOOGLE_GENAI_USE_VERTEXAI=$'0' gemini`,
	`GOOGLE_GENAI_USE_GCA=$'0' gemini`,
	// The env-wrapper form runs through the same selector parser, so the
	// same dollar-quoted values must fail closed there too.
	`env CLAUDE_CODE_USE_BEDROCK=$'0' claude`,
	`env CLAUDE_CODE_USE_BEDROCK=$"0" claude`,
	`/usr/bin/env CLAUDE_CODE_USE_BEDROCK=$'false' claude`,
	`env CLAUDE_CODE_USE_BEDROCK=$'0'$'0' claude`,
}

// selectorForCommand maps a command in commandEnvironmentFlagStateDollarQuoteCases
// to the (agent, selector) pair the test should consult. It picks the
// selector the command actually mentions; the test setup asserts one is
// found.
func selectorForCommand(command string) (agent, selector string) {
	for _, a := range []string{"claude", "gemini"} {
		for _, group := range conditionalAgentNames[a] {
			if strings.Contains(command, group.selector) {
				return a, group.selector
			}
		}
	}
	// Fallback: the command mentions claude by name and no conditional
	// selector, but the test corpus is constructed to mention one.
	if strings.Contains(command, "claude") {
		return "claude", "CLAUDE_CODE_USE_BEDROCK"
	}
	if strings.Contains(command, "gemini") {
		return "gemini", "GOOGLE_GENAI_USE_VERTEXAI"
	}
	return "", ""
}

// TestCommandEnvironmentFlagStateFailsClosedOnDollarQuotedSelectorValues
// is the direct pin on the fixed behavior: dialect-dependent selector
// assignment values must yield (found=false, enabled=false) for every
// cloud-mode selector on every agent, in both the bare and env-wrapped
// shapes. commandEnvironmentFlagState is the one place the fix lives;
// every public surface (ResolveAuthSelectors,
// CommandEnablesCloudCredentials, FilterForCommand) flows through it,
// and any future regression of the dual-variant check shows up here
// before it reaches any of them.
func TestCommandEnvironmentFlagStateFailsClosedOnDollarQuotedSelectorValues(t *testing.T) {
	for _, command := range commandEnvironmentFlagStateDollarQuoteCases {
		agent, selector := selectorForCommand(command)
		require.NotEmpty(t, selector, "test setup: %q must name a guarded selector", command)
		found, enabled := commandEnvironmentFlagState(command, agent, selector)
		require.Falsef(t, found, "commandEnvironmentFlagState(%q) found=true; a dialect-dependent dollar-quoted value must not even register as the selector's last assignment — it must fail closed as disabled", command)
		require.Falsef(t, enabled, "commandEnvironmentFlagState(%q) enabled=true; a dialect-dependent dollar-quoted value must fail closed as disabled, not enable the credential group", command)
	}
}

// TestFilterForCommandFailsClosedOnDollarQuotedCloudModeSelectors pins
// the end-to-end consequence: with no ambient selector set, a
// dollar-quoted assignment in the command line must not admit the
// provider's credential group into the filtered environment. The bug
// injected AWS / GCP / Azure keys into a session that ran with the
// cloud mode off; the fix admits nothing, across every provider
// group. FilterForCommand is the boundary used by the tmux launcher
// and the exec shim, so this is the public surface a launch actually
// reads.
func TestFilterForCommandFailsClosedOnDollarQuotedCloudModeSelectors(t *testing.T) {
	// Every provider's credential group is present in the source. The
	// filter must keep all of them out, regardless of which selector the
	// dollar-quoted value names.
	source := []string{
		"AWS_ACCESS_KEY_ID=fixture",
		"AWS_SECRET_ACCESS_KEY=fixture",
		"AWS_SESSION_TOKEN=fixture",
		"ANTHROPIC_VERTEX_PROJECT_ID=fixture",
		"GOOGLE_APPLICATION_CREDENTIALS=fixture",
		"GOOGLE_CLOUD_PROJECT=fixture",
		"AZURE_CLIENT_SECRET=fixture",
		"AZURE_CLIENT_ID=fixture",
	}
	for _, command := range commandEnvironmentFlagStateDollarQuoteCases {
		agent, _ := selectorForCommand(command)
		require.NotEmpty(t, agent, "test setup: %q must name an agent", command)
		got := FilterForCommand(source, agent, command, nil)
		for _, denied := range source {
			require.Falsef(t, slices.Contains(got, denied),
				"FilterForCommand admitted %s for dollar-quoted command %q — a dialect-dependent value must not inject a provider credential group",
				strings.SplitN(denied, "=", 2)[0], command)
		}
	}
}

// TestResolveAuthSelectorsAgreesWithCommandEnablesCloudCredentialsOnDollarQuotes
// extends config.TestCommandEnablesCloudCredentialsAgreesWithSelectorResolution
// to the dialect-dependent set. The anti-drift lock the cross-package
// test enforces on the plain cases must hold here too: any disagreement
// would reopen the silent injection the bug report names, on either
// side. ResolveAuthSelectors admits the credential group at the
// launching boundary; CommandEnablesCloudCredentials refuses checked-in
// program_overrides values. A dollar-quoted value the two surfaces
// classified differently would either let a repo check one in to take
// the operator's credentials, or inject them at launch while the
// repo-guard refused — exactly the bug, on either side.
func TestResolveAuthSelectorsAgreesWithCommandEnablesCloudCredentialsOnDollarQuotes(t *testing.T) {
	for _, command := range commandEnvironmentFlagStateDollarQuoteCases {
		agent, _ := selectorForCommand(command)
		selectors := ResolveAuthSelectors(nil, agent, command)
		_, guarded := CommandEnablesCloudCredentials(command)
		require.Zero(t, len(selectors),
			"ResolveAuthSelectors enabled %v for %q while CommandEnablesCloudCredentials reports %v — the two surfaces disagree on a dollar-quoted value",
			selectors, command, guarded)
		require.False(t, guarded,
			"CommandEnablesCloudCredentials reports %q as enabled for %q — a dollar-quoted value must not be cloud-credential-enabling",
			command, command)
	}
}

// TestExecInvocationFailsClosedOnDollarQuotedCloudModeSelector is the
// end-to-end guard at the actual launch boundary. The pane exec shim
// (execInvocation in exec_unix.go) hands /bin/sh -c the operator's
// command after FilterForCommand has built the environment, so the
// environment argument processExec receives IS the boundary's output.
// Mock processExec and inspect it: a dollar-quoted falsy selector value
// must hand /bin/sh -c an environment that carries no AWS / GCP / Azure
// credential, on any host — bash-as-/bin/sh would otherwise run the
// session with Bedrock OFF while the AWS credential group is present.
func TestExecInvocationFailsClosedOnDollarQuotedCloudModeSelector(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AWS_SESSION_TOKEN", "fixture")
	t.Setenv("AZURE_CLIENT_SECRET", "fixture")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "fixture")
	// Bare claude on PATH so the bare-name grant predicate attributes
	// it to the operator's PATH (operator's PATH = a tempdir af owns).
	pathDir := t.TempDir()
	writeExecutableFixture(t, pathDir, "claude")
	t.Setenv("PATH", pathDir)

	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	err := execInvocation([]string{"claude", "0", "CLAUDE_CODE_USE_BEDROCK=$'0' claude"}, false)
	require.ErrorIs(t, err, wantErr)

	for _, denied := range []string{
		"AWS_ACCESS_KEY_ID=fixture",
		"AWS_SECRET_ACCESS_KEY=fixture",
		"AWS_SESSION_TOKEN=fixture",
		"AZURE_CLIENT_SECRET=fixture",
		"GOOGLE_APPLICATION_CREDENTIALS=fixture",
	} {
		require.Falsef(t, slices.Contains(gotEnvironment, denied),
			"exec shim admitted %s into the environment handed to /bin/sh -c for a dollar-quoted falsy selector value — "+
				"the session would run with Bedrock off (bash-as-/bin/sh expands $'0' to 0) while carrying the AWS credential group",
			denied)
	}
}

// TestApplyAccountDoesNotFalselyReportCloudModeActiveOnDollarQuotedSelectors
// pins the account-scoped launch interaction the bug report names: a
// dollar-quoted falsy selector value made every account-scoped launch
// fail with a false "cloud mode … is active" error for a mode that was
// off. After the fix, ResolveAuthSelectors returns no selectors for
// these commands, so the cloud-mode-active refusal no longer fires.
// The account scope is still refused for the same reason a plain
// `CLAUDE_CODE_USE_BEDROCK=0 claude` override is refused — a
// command-local assignment to an identity-bearing variable would
// override the account directory, which is the documented reason —
// but it stops naming the cloud mode the operator wrote `=` to turn
// OFF.
func TestApplyAccountDoesNotFalselyReportCloudModeActiveOnDollarQuotedSelectors(t *testing.T) {
	for _, command := range []string{
		`CLAUDE_CODE_USE_BEDROCK=$'0' claude`,
		`CLAUDE_CODE_USE_BEDROCK=$'false' claude`,
		`CLAUDE_CODE_USE_VERTEX=$'0' claude`,
		`CLAUDE_CODE_USE_FOUNDRY=$'0' claude`,
		`env CLAUDE_CODE_USE_BEDROCK=$'0' claude`,
	} {
		_, err := ApplyAccount(nil, command, Account{Agent: "claude", Name: "work", Dir: "/afhome/accounts/claude/work"})
		require.Error(t, err, "%q must still be refused — any command-local assignment to an identity-bearing variable overrides the account scope")
		require.NotContainsf(t, err.Error(), "cloud mode",
			"ApplyAccount refused %q with a false \"cloud mode is active\" message; a dialect-dependent dollar-quoted value must not register a mode as active", command)
	}
}

// TestApplyAccountStillRefusesOnPlainEnabledCloudModeSelectors fills the
// gap the existing TestApplyAccount_RefusesWhileACloudModeIsActive leaves:
// that test pins the env-set selector case (`env := []string{"NAME=1"}`),
// not the command-local assignment (`NAME=1 claude`). A future change
// that only broke the command-local enablement path would slip past it.
// The fix touches commandEnvironmentFlagState, which is the command-local
// path, so this asserts the command-local `=1` case still refuses the
// account scope with "cloud mode" in the error.
func TestApplyAccountStillRefusesOnPlainEnabledCloudModeSelectors(t *testing.T) {
	for _, command := range []string{
		`CLAUDE_CODE_USE_BEDROCK=1 claude`,
		`CLAUDE_CODE_USE_VERTEX=true claude`,
		`CLAUDE_CODE_USE_FOUNDRY=1 claude`,
		`env CLAUDE_CODE_USE_BEDROCK=1 claude`,
	} {
		_, err := ApplyAccount(nil, command, Account{Agent: "claude", Name: "work", Dir: "/afhome/accounts/claude/work"})
		require.ErrorContainsf(t, err, "cloud mode",
			"ApplyAccount accepted %q — a plain literal enabling value must still refuse the account scope", command)
	}
}

// TestDollarQuotedSelectorValueDivergesBetweenShells is the live oracle
// for the bug's premise: a dollar-quoted selector value's shell expansion
// is dialect-dependent in the field. Where /bin/sh is bash (macOS, RHEL/
// Fedora, Arch), `$'0'` becomes 0 (falsy — mode OFF). Where /bin/sh is
// dash (Debian/Ubuntu), `$'0'` is the literal two characters `$0` (truthy).
// The session's actual environment therefore depends on which /bin/sh the
// host ships, while af's POSIX-only literalization happened to agree with
// dash — and so injected credentials on the bash-as-sh hosts. This test
// documents WHY the fail-closed treatment exists; if a future shell
// convergence ever makes the expansion literal in both, this test starts
// skipping or fails, prompting a re-evaluation. Skip on hosts without
// bash and an independent /bin/sh to compare against.
func TestDollarQuotedSelectorValueDivergesBetweenShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pane launches through /bin/sh only on unix")
	}
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH to validate the dialect divergence; the parser-path tests already pin the fix")
	}
	shPath, err := exec.LookPath("sh")
	if err != nil {
		// /bin/sh specifically, since that is the resolver the exec shim uses.
		if _, statErr := os.Stat("/bin/sh"); statErr != nil {
			t.Skipf("no /bin/sh to compare against: %v", statErr)
		}
		shPath = "/bin/sh"
	}
	// Resolve any symlink so a bash-as-/bin/sh host (where the two are the
	// same shell) is detected: there is no divergence there. The bug
	// silently agreed with the running shell on those hosts that DO
	// support ANSI-C quoting but happens to host dash as /bin/sh — that
	// is not this test's claim. The claim is that bash and dash differ,
	// and that af cannot tell which /bin/sh the operator's host has.
	shReal, shErr := filepath.EvalSymlinks(shPath)
	bashReal, bashErr := filepath.EvalSymlinks(bashPath)
	if shErr == nil && bashErr == nil && shReal == bashReal {
		t.Skipf("/bin/sh and bash resolve to the same binary (%s); the divergence the bug relies on is not observable here", shReal)
	}
	for _, tc := range []struct {
		name    string
		command string
	}{
		{name: "ANSI-C single quote falsy zero", command: `x=$'0'; printf '%s' "$x"`},
		{name: "ANSI-C single quote falsy false", command: `x=$'false'; printf '%s' "$x"`},
		{name: "locale double quote falsy zero", command: `x=$"0"; printf '%s' "$x"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shOut, shErr := exec.Command(shPath, "-c", tc.command).Output()
			bashOut, bashErr := exec.Command(bashPath, "-c", tc.command).Output()
			require.NoError(t, shErr, "running %q under %s failed", tc.command, shPath)
			require.NoError(t, bashErr, "running %q under bash failed", tc.command)
			// The bug's premise: the two dialects expand the same source
			// text to different bytes. bash supports ANSI-C / locale
			// quoting (so $'0' -> 0); dash does not (so $'0' -> $0).
			require.NotEqualf(t, string(bashOut), string(shOut),
				"the live oracle found no divergence between bash (%s) and %s (%s) for %q; the bug report's premise does not hold in this environment",
				bashPath, shPath, shReal, tc.command)
			// And confirm bash — which is what macOS and several major
			// Linux distributions ship as /bin/sh — expands the falsy
			// form to the falsy string itself, so the running process
			// has the cloud mode OFF while af's POSIX-only parser
			// literalized it as truthy. flagValueEnabled is the same
			// rule the selector path uses to classify the literalized
			// value, so the bash output must be falsy by it.
			require.Truef(t, !flagValueEnabled(string(bashOut)),
				"bash expanded the dollar-quoted form to %q, which flagValueEnabled does not classify as falsy; the bug's premise (the running process has the mode OFF under bash-as-sh) needs re-examination for %q",
				string(bashOut), tc.command)
		})
	}
}
