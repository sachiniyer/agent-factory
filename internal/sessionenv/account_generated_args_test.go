package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// af's real generated claude launch, as the local backend produces it: the
// conversation injection appends `--session-id <uuid>` and the guidance seam
// appends `--plugin-dir <dir>`. Written as the pair the launcher would declare,
// so a test cannot accidentally agree with the guard by using a simplified shape.
const (
	genSessionID = "0b6f2c1e-8a44-4d0e-9f31-7c5b2a9e4d10"
	genPluginDir = "/home/op/.local/share/agent-factory/plugins/af"
)

func claudeAccount(generated ...string) Account {
	return Account{
		Agent:         "claude",
		Name:          "work",
		Dir:           "/afhome/accounts/claude/work",
		GeneratedArgs: generated,
	}
}

func afGeneratedClaudeArgs() []string {
	return []string{"--session-id", genSessionID, "--plugin-dir", genPluginDir}
}

// THE BUG (#3083): the guard rejected arguments af itself authored, so an
// account-scoped claude session exited 127.
//
// Declaring them makes the launch provable, and the assertion is the whole point
// of the feature rather than the guard's boolean: the account's directory is what
// the session sees, and the ambient key that would outrank it is gone.
func TestApplyAccount_AcceptsAfsOwnGeneratedClaudeLaunch(t *testing.T) {
	command := "claude --session-id " + genSessionID + " --plugin-dir " + genPluginDir
	ambient := []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-ambient", "HOME=/home/op"}

	scoped, err := ApplyAccount(ambient, command, claudeAccount(afGeneratedClaudeArgs()...))
	require.NoError(t, err,
		"af generated --session-id and --plugin-dir itself, so the guard was refusing its own output")

	dir, ok := envValue(scoped, "CLAUDE_CONFIG_DIR")
	require.True(t, ok, "the account's credential root must be injected")
	require.Equal(t, "/afhome/accounts/claude/work", dir)
	_, leaked := envValue(scoped, "ANTHROPIC_API_KEY")
	require.False(t, leaked,
		"an ambient API key outranks the config directory, so leaving it makes the account selection silently a no-op")
}

// The SAME launch with nothing declared is still refused, which is the behaviour
// before this field existed. The mechanism widens what af can prove about its own
// output; it does not loosen the rule for anyone else.
func TestApplyAccount_UndeclaredArgumentsAreStillRefused(t *testing.T) {
	command := "claude --session-id " + genSessionID + " --plugin-dir " + genPluginDir
	_, err := ApplyAccount([]string{"PATH=/usr/bin"}, command, claudeAccount())
	require.Error(t, err, "arguments nobody vouched for are not provable just because they look like af's")
	require.Contains(t, err.Error(), "could not be proven")
}

// THE TEST THAT SEPARATES PROVENANCE FROM AN ALLOWLIST, and the reason this is
// not a list of permitted flags.
//
// A repository can write anything into program_overrides, including the exact
// flag NAMES af uses. What it cannot do is know the uuid af minted for this
// launch or the plugin directory af resolved. So the comparison is against the
// VALUES the launcher generated, whole — which means a flag-name match buys an
// attacker nothing.
//
// An allowlist of `--session-id` and `--plugin-dir` would accept every one of
// these, and `--plugin-dir` in particular hands claude a repository-chosen
// directory of plugin code.
func TestApplyAccount_GeneratedArgsAreValuesNotFlagNames(t *testing.T) {
	generated := afGeneratedClaudeArgs()
	for _, test := range []struct {
		name    string
		command string
		why     string
	}{
		{
			name:    "a different session id",
			command: "claude --session-id attacker-chosen --plugin-dir " + genPluginDir,
			why:     "the flag name matches and the value does not; only the value is evidence af wrote it",
		},
		{
			name:    "a different plugin dir",
			command: "claude --session-id " + genSessionID + " --plugin-dir /repo/evil-plugins",
			why:     "--plugin-dir loads repository code into claude; accepting the flag regardless of value is the allowlist mistake",
		},
		{
			name:    "an extra argument after the declared ones",
			command: "claude --session-id " + genSessionID + " --plugin-dir " + genPluginDir + " --settings /repo/auth.json",
			why:     "claude's --settings can redirect auth; the declared suffix must be the WHOLE argument list, not a prefix of it",
		},
		{
			name:    "an extra argument before the declared ones",
			command: "claude --settings /repo/auth.json --session-id " + genSessionID + " --plugin-dir " + genPluginDir,
			why:     "anchoring only the tail would let anything sit in front of it",
		},
		{
			name:    "the declared words reordered",
			command: "claude --plugin-dir " + genPluginDir + " --session-id " + genSessionID,
			why:     "position is part of the claim; reordering is a different command than the one described",
		},
		{
			name:    "fewer words than declared",
			command: "claude --session-id " + genSessionID,
			why:     "the command is not the one the launcher described, so its description does not apply to it",
		},
		{
			name:    "a value that is a command substitution",
			command: "claude --session-id " + genSessionID + " --plugin-dir \"$(cat /repo/x)\"",
			why:     "/bin/sh expands this BEFORE claude starts, so the text that matches is not the word af generated",
		},
		{
			name:    "an identity assignment in front of a correctly declared suffix",
			command: "ANTHROPIC_API_KEY=sk-repo claude --session-id " + genSessionID + " --plugin-dir " + genPluginDir,
			why:     "provenance for the arguments says nothing about an assignment prefix, which outranks the account directory",
		},
		{
			name:    "a different agent carrying claude's declared suffix",
			command: "codex --session-id " + genSessionID + " --plugin-dir " + genPluginDir,
			why:     "CLAUDE_CONFIG_DIR means nothing to codex, so it would authenticate from its own default home",
		},
		{
			name:    "a path-qualified agent with a correct suffix",
			command: "./claude --session-id " + genSessionID + " --plugin-dir " + genPluginDir,
			why:     "a basename is not provenance; ./claude is an arbitrary repository file handed the account root",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ApplyAccount([]string{"PATH=/usr/bin"}, test.command, claudeAccount(generated...))
			require.Error(t, err, test.why)
		})
	}
}

// The `env` wrapper must reach the SAME rule, not a laxer one. It is the one
// wrapper this guard models, so it is also the obvious place for the provenance
// check to be forgotten.
func TestApplyAccount_EnvWrapperAppliesTheSameProvenanceRule(t *testing.T) {
	generated := afGeneratedClaudeArgs()
	suffix := " --session-id " + genSessionID + " --plugin-dir " + genPluginDir

	scoped, err := ApplyAccount([]string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-ambient"},
		"env claude"+suffix, claudeAccount(generated...))
	require.NoError(t, err, "env around af's own generated launch is the same provable shape")
	dir, ok := envValue(scoped, "CLAUDE_CONFIG_DIR")
	require.True(t, ok)
	require.Equal(t, "/afhome/accounts/claude/work", dir)

	for _, test := range []struct{ name, command, why string }{
		{"a mutation through env", "env ANTHROPIC_API_KEY=sk-repo claude" + suffix,
			"env-borne assignments recreate an identity that outranks the account directory"},
		{"an undeclared value through env", "env claude --session-id attacker --plugin-dir " + genPluginDir,
			"the env branch must compare values too, or it becomes the laxer path"},
		{"an extra operand through env", "env claude" + suffix + " --settings /repo/auth.json",
			"the declared suffix is the whole operand list here as well"},
		{"an env option in front", "env -i claude" + suffix,
			"-i drops the injected root entirely"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ApplyAccount([]string{"PATH=/usr/bin"}, test.command, claudeAccount(generated...))
			require.Error(t, err, test.why)
		})
	}
}

// A declared suffix must not become a way to smuggle arguments past the
// no-arguments rule for an agent af did NOT generate a launch for. Codex takes no
// generated arguments today, so an empty declaration must keep refusing them —
// this is the regression guard for "someone wires GeneratedArgs globally".
func TestApplyAccount_EmptyDeclarationKeepsTheNoArgumentRule(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}

	scoped, err := ApplyAccount([]string{"PATH=/usr/bin", "OPENAI_API_KEY=sk-ambient"}, "codex", account)
	require.NoError(t, err, "a bare invocation is the shape that was always provable")
	_, leaked := envValue(scoped, "OPENAI_API_KEY")
	require.False(t, leaked)

	_, err = ApplyAccount([]string{"PATH=/usr/bin"},
		`codex -c cli_auth_credentials_store="keyring"`, account)
	require.Error(t, err,
		"this makes codex ignore the account's auth.json for the machine-wide keyring; an empty declaration "+
			"must leave that refusal exactly as it was")
}

// The producer and the guard must agree by construction, so the round trip is the
// assertion: whatever GeneratedArgsBetween reports for a rewrite, ApplyAccount
// must accept for the rewritten command. A test that hand-writes the declaration
// proves the guard works on a list somebody typed, not on af's own output.
func TestGeneratedArgsBetween_RoundTripsThroughTheGuard(t *testing.T) {
	base := "claude"
	final := "claude --session-id " + genSessionID + " --plugin-dir " + genPluginDir

	generated, ok := GeneratedArgsBetween(base, final)
	require.True(t, ok)
	require.Equal(t, afGeneratedClaudeArgs(), generated)

	_, err := ApplyAccount([]string{"PATH=/usr/bin"}, final, claudeAccount(generated...))
	require.NoError(t, err, "the launcher's own description of its rewrite must satisfy the guard")
}

// A program_overrides base keeps working: af appends to whatever the operator
// configured, and only af's addition is declared. The base still has to pass the
// guard's own rules, so this pair is refused for the base — which is correct, and
// is why the declaration cannot launder a bad base.
func TestGeneratedArgsBetween_DescribesOnlyTheAddition(t *testing.T) {
	generated, ok := GeneratedArgsBetween("claude --settings /repo/auth.json",
		"claude --settings /repo/auth.json --session-id "+genSessionID)
	require.True(t, ok, "the difference is computable without judging what the words mean")
	require.Equal(t, []string{"--session-id", genSessionID}, generated)

	_, err := ApplyAccount([]string{"PATH=/usr/bin"},
		"claude --settings /repo/auth.json --session-id "+genSessionID, claudeAccount(generated...))
	require.Error(t, err,
		"the operator's own --settings is NOT af-authored, so it remains unprovable — declaring af's "+
			"addition must not vouch for the base it was appended to")
}

func TestGeneratedArgsBetween_FailsClosed(t *testing.T) {
	for _, test := range []struct{ name, base, final, why string }{
		{"an edited leading word", "claude", "claude-next --session-id x",
			"the executable changed, so this is not an append"},
		{"a changed user argument", "claude --model a", "claude --model b --session-id x",
			"a rewrite that edits an existing word cannot honestly be described as an addition"},
		{"final shorter than base", "claude --model a", "claude",
			"nothing was appended; something was removed"},
		{"a command substitution in the addition", "claude", "claude --plugin-dir \"$(cat /x)\"",
			"the word af generated is not the text that survives expansion"},
		{"two commands", "claude", "claude --session-id x; evil",
			"not a single simple command"},
		{"an assignment prefix", "claude", "ANTHROPIC_API_KEY=sk claude --session-id x",
			"an assignment is not an appended argument, and this one recreates an identity"},
		{"an empty base", "", "claude --session-id x", "there is no base to append to"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, ok := GeneratedArgsBetween(test.base, test.final)
			require.False(t, ok, test.why)
		})
	}
}

// Identical strings are the ordinary non-claude launch: af appends nothing, and
// the empty declaration must keep the no-arguments rule intact rather than being
// mistaken for "unknown".
func TestGeneratedArgsBetween_NoRewriteDeclaresNothing(t *testing.T) {
	generated, ok := GeneratedArgsBetween("codex", "codex")
	require.True(t, ok)
	require.Empty(t, generated)
}

func TestGenerateAccountLaunchProof_TrustsOnlyTheBuiltInBase(t *testing.T) {
	const executable = "/opt/af-detected/claude"
	base := executable + " --dangerously-skip-permissions"
	final := base + " --session-id " + genSessionID

	builtIn, ok := GenerateAccountLaunchProof(base, final, []string{"--dangerously-skip-permissions"})
	require.True(t, ok)
	require.Equal(t, executable, builtIn.TrustedExecutable)
	require.Equal(t, []string{"--dangerously-skip-permissions", "--session-id", genSessionID}, builtIn.GeneratedArgs)
	require.NoError(t, ValidateAccountCommand(final, Account{
		Agent:             "claude",
		Name:              "work",
		TrustedExecutable: builtIn.TrustedExecutable,
		GeneratedArgs:     builtIn.GeneratedArgs,
	}))

	aliasBase := executable + " --settings /operator/auth.json --dangerously-skip-permissions"
	aliasFinal := aliasBase + " --session-id " + genSessionID
	aliasProof, ok := GenerateAccountLaunchProof(aliasBase, aliasFinal,
		[]string{"--dangerously-skip-permissions"})
	require.True(t, ok)
	err := ValidateAccountCommand(aliasFinal, Account{
		Agent: "claude", Name: "work", TrustedExecutable: aliasProof.TrustedExecutable,
		GeneratedArgs: aliasProof.GeneratedArgs,
	})
	require.ErrorContains(t, err, "--settings",
		"arguments from the detected shell alias are operator-authored, not af-authored")
	require.ErrorContains(t, err, "/operator/auth.json")

	userOverride, ok := GenerateAccountLaunchProof("claude --model sonnet",
		"claude --model sonnet --session-id "+genSessionID, nil)
	require.True(t, ok)
	require.Empty(t, userOverride.TrustedExecutable)
	require.Equal(t, []string{"--session-id", genSessionID}, userOverride.GeneratedArgs,
		"a user override's arguments must not be laundered into af-authored provenance")
	err = ValidateAccountCommand("claude --model sonnet --session-id "+genSessionID, Account{
		Agent: "claude", Name: "work", GeneratedArgs: userOverride.GeneratedArgs,
	})
	require.ErrorContains(t, err, "--model")
	require.ErrorContains(t, err, "sonnet")
}

func TestGenerateAccountLaunchProof_DoesNotTrustRelativeDetectedExecutable(t *testing.T) {
	base := "./bin/claude --dangerously-skip-permissions"
	final := base + " --session-id " + genSessionID

	proof, ok := GenerateAccountLaunchProof(base, final, []string{"--dangerously-skip-permissions"})
	require.True(t, ok)
	require.Empty(t, proof.TrustedExecutable,
		"a relative executable is resolved again from the pane workdir and cannot inherit detection from the daemon cwd")
	require.Equal(t, []string{"--dangerously-skip-permissions", "--session-id", genSessionID}, proof.GeneratedArgs,
		"rejecting executable provenance must not lose the independently known af-authored words")
	err := ValidateAccountCommand(final, Account{
		Agent: "claude", Name: "work", TrustedExecutable: proof.TrustedExecutable,
		GeneratedArgs: proof.GeneratedArgs,
	})
	require.Error(t, err, "the relative executable must fail closed before receiving the account directory")

	bareBase := "claude --dangerously-skip-permissions"
	bareFinal := bareBase + " --session-id " + genSessionID
	bareProof, ok := GenerateAccountLaunchProof(bareBase, bareFinal, []string{"--dangerously-skip-permissions"})
	require.True(t, ok)
	require.Empty(t, bareProof.TrustedExecutable,
		"the ordinary bare-name rule proves claude without manufacturing path provenance")
	require.NoError(t, ValidateAccountCommand(bareFinal, Account{
		Agent: "claude", Name: "work", TrustedExecutable: bareProof.TrustedExecutable,
		GeneratedArgs: bareProof.GeneratedArgs,
	}), "a bare agent name with only af-authored arguments remains safe")
}
