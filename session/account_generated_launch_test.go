package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// #3083's test bar, and it is a bar about the FIXTURE rather than the assertion:
// "a test that passes on a hand-written bare `claude` proves nothing about the
// path that actually runs."
//
// So this drives af's real generators — planLaunchConversation, which appends
// `--session-id <uuid>`, and injectSystemPrompt, which appends `--plugin-dir
// <dir>` after creating that directory — and scopes the command they produce.
// Nothing here writes a command string by hand, which is why it lives in package
// session: both generators are unexported, and a copy of their output in
// internal/sessionenv would be exactly the simplified shape the bar rules out.
func TestAccountScopesAfsRealGeneratedClaudeLaunch(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const base = "claude"
	withConversation, conversation := planLaunchConversation("11111111-2222-3333-4444-555555555555", base)
	command := injectSystemPrompt(withConversation, skillTarget{})

	// The fixture must be the REWRITTEN command, not a bare invocation that would
	// have passed before #3083 existed. Asserted rather than assumed: if either
	// generator stopped appending — a plugin-dir failure logs and returns the input
	// unchanged — this test would silently regress into the hand-written case the
	// bar rejects.
	require.NotEqual(t, base, command,
		"af's generators appended nothing, so this proves nothing about the account path that actually runs")
	require.Contains(t, command, "--session-id "+conversation.ID)
	require.Contains(t, command, "--plugin-dir ")

	generated, ok := sessionenv.GeneratedArgsBetween(base, command)
	require.True(t, ok, "the launcher must be able to describe its own rewrite")
	require.NotEmpty(t, generated)

	ambient := []string{
		"PATH=/usr/bin",
		"HOME=/home/op",
		"ANTHROPIC_API_KEY=sk-ambient-outranks-the-account-dir",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-ambient",
	}
	account := sessionenv.Account{
		Agent:         "claude",
		Name:          "work",
		Dir:           t.TempDir(),
		GeneratedArgs: generated,
	}

	scoped, err := sessionenv.ApplyAccount(ambient, command, account)
	require.NoError(t, err,
		"the pane exited 127 here (#3083): af generated --session-id and --plugin-dir, and the account "+
			"boundary refused its own output")

	require.Equal(t, account.Dir, envLookup(t, scoped, "CLAUDE_CONFIG_DIR"),
		"the session must see THIS account's credential root")
	require.NotContains(t, envNames(scoped), "ANTHROPIC_API_KEY",
		"an ambient API key takes precedence over CLAUDE_CONFIG_DIR, so leaving it makes the account "+
			"selection silently a no-op — the exact failure the feature exists to prevent")
	require.NotContains(t, envNames(scoped), "CLAUDE_CODE_OAUTH_TOKEN")

	// And the negative half against the SAME real command: without the declaration
	// it is still refused. This is what makes the positive result mean "provenance
	// was verified" rather than "the guard got looser".
	_, err = sessionenv.ApplyAccount(ambient, command, sessionenv.Account{
		Agent: account.Agent, Name: account.Name, Dir: account.Dir,
	})
	require.Error(t, err,
		"af's real generated command must remain unprovable when nothing vouches for the added arguments")
}

func envNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, kv := range env {
		if name, _, ok := strings.Cut(kv, "="); ok {
			names = append(names, name)
		}
	}
	return names
}

func envLookup(t *testing.T, env []string, name string) string {
	t.Helper()
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	t.Fatalf("%s is not present in the scoped environment", name)
	return ""
}

// The END-TO-END fix for #3083, through af's real launch seam rather than through
// ApplyAccount directly: setLaunchProgram is what every launch path now calls, so
// this asserts the declaration it installs is the one the boundary needs.
//
// It exercises the pairing rather than the guard — the guard is covered in
// internal/sessionenv — because the pairing is where this could silently break: a
// declaration describing a different program string than the one installed would be
// a false claim about the command that runs.
func TestSetLaunchProgram_DeclaresWhatTheAccountBoundaryNeeds(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	base := "claude"
	final := injectSystemPrompt(base, skillTarget{})
	require.NotEqual(t, base, final, "precondition: claude's launch is rewritten, or this proves nothing")

	generated, ok := sessionenv.GeneratedArgsBetween(base, final)
	require.True(t, ok)

	// What the pane shim will hand the boundary, and it must be accepted.
	_, err := sessionenv.ApplyAccount(
		[]string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-ambient"},
		final,
		sessionenv.Account{Agent: "claude", Name: "work", Dir: t.TempDir(), GeneratedArgs: generated},
	)
	require.NoError(t, err, "af's own launch rewrite must be provable end to end (#3083)")
}

// A base af cannot relate to the final command declares NOTHING, so an
// account-scoped launch is refused rather than proceeding unverified. This is the
// fail-closed direction, and it is the one a reader is most likely to assume works
// the other way.
func TestSetLaunchProgram_UnrelatableBaseDeclaresNothing(t *testing.T) {
	generated, ok := sessionenv.GeneratedArgsBetween("claude", "ANTHROPIC_API_KEY=sk claude --session-id x")
	require.False(t, ok, "an assignment prefix is not an appended argument")
	require.Empty(t, generated)

	_, err := sessionenv.ApplyAccount([]string{"PATH=/usr/bin"},
		"ANTHROPIC_API_KEY=sk claude --session-id x",
		sessionenv.Account{Agent: "claude", Name: "work", Dir: "/d", GeneratedArgs: generated})
	require.Error(t, err, "refused, not launched unverified")
}
