package tmux

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// Restore's re-spawn branch rewrites the program with resume flags AFTER the launch
// recorded which words af appended. Those flags are af-authored too, so the
// declaration has to grow with them — a stale one makes the account boundary refuse
// the restored launch and the pane exits 127, which is #3083 reappearing one path
// over (#3083 review, P1).
//
// Asserted against ApplyAccount rather than against the slice, because "the
// declaration matches the command" is the only property that matters and comparing
// slices would pass on a declaration that happened to be wrong in the same way.
func TestRewriteProgramCmdByAf_KeepsTheDeclarationInStepWithResume(t *testing.T) {
	const pluginDir = "/plugins/af"
	for _, test := range []struct {
		name      string
		program   string
		declared  []string
		agent     string
		wantAdded string
	}{
		{
			name:      "claude appends --continue",
			program:   "claude --session-id sid-1 --plugin-dir " + pluginDir,
			declared:  []string{"--session-id", "sid-1", "--plugin-dir", pluginDir},
			agent:     "claude",
			wantAdded: "--continue",
		},
		{
			name:      "codex splices resume --last onto a bare program",
			program:   "codex",
			declared:  nil,
			agent:     "codex",
			wantAdded: "--last",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &TmuxSession{program: test.program, generatedArgs: test.declared}

			session.rewriteProgramCmdByAf(resumeProgram)

			rewritten := session.programCmd()
			require.NotEqual(t, test.program, rewritten, "precondition: resume must have rewritten it")
			require.Contains(t, rewritten, test.wantAdded)

			// THE PROPERTY: the boundary accepts the command that will actually run.
			_, err := sessionenv.ApplyAccount(
				[]string{"PATH=/usr/bin"},
				rewritten,
				sessionenv.Account{
					Agent: test.agent, Name: "work", Dir: "/afhome/accounts/" + test.agent + "/work",
					GeneratedArgs: session.generatedArgs,
				},
			)
			require.NoError(t, err,
				"the resume rewrite left the declaration stale, so an account-scoped restore is refused "+
					"and the re-spawned pane exits 127")
		})
	}
}

// A rewrite that is not a positional extension CLEARS the declaration rather than
// leaving a false one. Refusing is honest; claiming af authored words it cannot
// account for is not.
func TestRewriteProgramCmdByAf_ClearsAnUndescribableRewrite(t *testing.T) {
	session := &TmuxSession{program: "claude", generatedArgs: []string{"--session-id", "sid-1"}}
	session.rewriteProgramCmdByAf(func(string) string { return "ANTHROPIC_API_KEY=sk claude --continue" })
	require.Empty(t, session.generatedArgs,
		"an assignment prefix is not an appended argument; the declaration must not survive it")
}

// A SOURCE guard, not a behavioural one, and labelled so nobody mistakes it for
// coverage of Restore itself.
//
// The helper above is unit-tested, but the thing that actually breaks is the CALL
// SITE: reverting Restore to `setProgramCmd(resumeProgram(...))` leaves every test
// here green while the declaration goes stale again. Binding that properly needs a
// real re-spawn against real tmux, which this suite can do but only by spawning a
// server for a one-line assertion.
//
// So this asserts the drift-prone spelling is absent. It is weak — it proves a
// string is not present, not that the behaviour is right — and it is here because
// the alternative was to claim the call site was covered when it was not.
func TestRestoreUsesTheDeclarationPreservingRewrite(t *testing.T) {
	source, err := os.ReadFile("start.go")
	require.NoError(t, err)
	require.NotContains(t, string(source), "setProgramCmd(resumeProgram(",
		"Restore must rewrite the program through rewriteProgramCmdByAf, which moves the generated-args "+
			"declaration with it; setProgramCmd alone leaves the declaration stale and an account-scoped "+
			"re-spawn then exits 127 (#3083 review)")
	require.Contains(t, string(source), "rewriteProgramCmdByAf(resumeProgram)",
		"and it must still be doing that rewrite at all")
}
