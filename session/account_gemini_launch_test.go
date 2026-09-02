package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// The launch proof #3639 verified, pinned as code.
//
// `refuseUnsupportedAccountAgent` is an allowlist of agents whose LAUNCH the
// account boundary has been shown to accept — a different question from whether
// the agent has a credential-root variable, which is what put gemini on the roster
// in #3387. gemini is admitted now because af hands its pane the resolved command
// UNCHANGED: planLaunchConversation returns early for anything that is not claude,
// and injectSystemPrompt's gemini arm writes the af skill and returns the command
// untouched. That is codex's shape, not claude's, and it is provable without any
// generated-argument declaration.
//
// This test exists because that property is INCIDENTAL to code in three other
// files. Give gemini a `--session-id` injection like claude's, or an af-authored
// flag in injectSystemPrompt, and the boundary starts refusing af's own output —
// the pane then exits 127 for every account-scoped gemini session, which is #3083
// exactly. Asserting against ValidateAccountCommand rather than against the string
// is deliberate: the property is "the boundary accepts what will actually run".
func TestGeminiLaunch_ProducesACommandTheAccountBoundaryAccepts(t *testing.T) {
	// Hermetic: injectSystemPrompt's gemini arm resolves a skills directory from
	// GEMINI_CLI_HOME and consults the af config, and neither may touch the
	// operator's real home from a test.
	t.Setenv("GEMINI_CLI_HOME", t.TempDir())
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const resolved = "gemini"
	program, conversation := planLaunchConversation("11111111-2222-3333-4444-555555555555", resolved)
	require.Equal(t, resolved, program,
		"conversation planning must leave a non-claude command alone; an injected --session-id would be an "+
			"af-authored argument the account boundary has to be told about")
	require.Empty(t, conversation.ID, "gemini has no af-managed conversation id")

	program = injectSystemPrompt(program)
	require.Equal(t, resolved, program,
		"injectSystemPrompt's gemini arm writes the af skill through the filesystem and returns the command "+
			"untouched; a flag added here would have to be declared as a generated argument too")

	proof := accountLaunchProof(resolved, program, false)
	require.Empty(t, proof.GeneratedArgs, "af authored no arguments for gemini, so it declares none")
	require.Empty(t, proof.TrustedExecutable, "gemini has no built-in detected override to trust")

	require.NoError(t, sessionenv.ValidateAccountCommand(program, sessionenv.Account{
		Agent:             "gemini",
		Name:              "work",
		Dir:               "/afhome/accounts/gemini/work",
		TrustedExecutable: proof.TrustedExecutable,
		GeneratedArgs:     proof.GeneratedArgs,
	}), "the account boundary must accept the exact command the create path installs")
}

// The container backend must neutralize exactly what the local shim does.
//
// #3609 made ApplyAccount leave gemini's identity names DEFINED AND EMPTY rather
// than removing them, because gemini reads a repository `.env` after af's boundary
// has run. docker installs `-e NAME=` for the same set, and it reads that set from
// the shared classification — so a name the shim pins and this list omits is a
// container session with a hole the local one does not have.
//
// The blank set is a UNION of two independent classifications
// (AccountIdentityNames and AgentAuthSelectors), and this asserts the union rather
// than either half: dropping GOOGLE_APPLICATION_CREDENTIALS from the credential
// names alone leaves it covered by the pinned set and vice versa, so what this
// catches is a name that stops being classified anywhere — which is exactly the
// state that puts it back in the container.
func TestAccountMountAndEnv_BlanksEveryGeminiIdentity(t *testing.T) {
	_, env, err := accountMountAndEnv(sessionenv.Account{
		Agent: "gemini", Name: "work", Dir: "/acct/gemini/work",
	}, false)
	require.NoError(t, err)

	for _, name := range []string{
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA",
	} {
		got, found := dockerEnvArg(env, name)
		require.True(t, found, "%s must be installed in the container, not merely absent from it: "+
			"gemini's own `.env` loader assigns any name it does not already find", name)
		require.Equal(t, name+"=", got, "%s must be blank, never carry a value", name)
	}

	got, found := dockerEnvArg(env, "GEMINI_CLI_HOME")
	require.True(t, found)
	require.Equal(t, "GEMINI_CLI_HOME="+dockerAccountHome, got,
		"the config variable is injected with the container's account path, never blanked")
}
