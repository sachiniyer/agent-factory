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
// SITE: reverting Restore to a bare setter around resumeProgram leaves every test
// here green while the declaration goes stale again. Binding that properly needs a
// real re-spawn against real tmux, which this suite can do but only by spawning a
// server for a one-line assertion.
//
// The HALF of that this used to assert — that `setProgramCmd(resumeProgram(` is
// absent from start.go — is now the compiler's, because #3734 deleted
// setProgramCmd: it survived the #3083 rewrite with no caller at all, which is
// to say the only thing left reaching for it was this string. A spelling that
// does not compile cannot drift back, so the string check is kept only as a
// tripwire against someone reintroducing the setter, and the assertion that
// still carries weight is the positive one below.
//
// It remains weak — it proves a string is present, not that the behaviour is
// right — and it is here because the alternative was to claim the call site was
// covered when it was not.
func TestRestoreUsesTheDeclarationPreservingRewrite(t *testing.T) {
	source, err := os.ReadFile("start.go")
	require.NoError(t, err)
	require.NotContains(t, string(source), "setProgramCmd(resumeProgram(",
		"Restore must rewrite the program through rewriteProgramCmdByAf, which moves the generated-args "+
			"declaration with it; a bare setter leaves the declaration stale and an account-scoped "+
			"re-spawn then exits 127 (#3083 review). setProgramCmd was removed in #3734, so reaching "+
			"this assertion means it was reintroduced")
	require.Contains(t, string(source), "rewriteProgramCmdByAf(resumeProgram)",
		"and it must still be doing that rewrite at all")
}

// The command and its declaration must be observable only as a PAIR (#3083
// review, P2). Two independently-locked setters, or a launch that reads the
// program through one lock and the declaration through another, let a concurrent
// rewrite hand the boundary an old command with a new declaration — and the
// boundary requires the command to be the agent plus EXACTLY the declared words,
// so either half of that tear refuses the launch and the pane exits at once.
//
// Run under -race, with a rewriter racing a reader: any snapshot that pairs a
// command with a declaration that does not describe it fails.
func TestLaunchSnapshot_NeverTearsTheCommandFromItsDeclaration(t *testing.T) {
	session := &TmuxSession{}
	session.SetLaunchProgram("claude", sessionenv.AccountLaunchProof{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			session.SetLaunchProgram("claude", sessionenv.AccountLaunchProof{})
			session.rewriteProgramCmdByAf(resumeProgram)
		}
	}()
	for i := 0; i < 500; i++ {
		program, proof, _, _, _, _, _ := session.launchSnapshot()
		// Every legal pair: bare with no declaration, or resumed with --continue
		// declared. A torn read produces one without the other.
		switch program {
		case "claude":
			require.Empty(t, proof.GeneratedArgs, "a bare command was paired with a declaration describing a rewrite")
		case "claude --continue":
			require.Equal(t, []string{"--continue"}, proof.GeneratedArgs,
				"the resumed command was paired with a declaration that does not describe it")
		default:
			t.Fatalf("unexpected program %q", program)
		}
	}
	<-done
}

func TestLaunchSnapshot_NeverTearsTrustedExecutableFromCommand(t *testing.T) {
	const trusted = "/opt/af-detected/claude"
	session := &TmuxSession{}
	detectedProof := sessionenv.AccountLaunchProof{
		TrustedExecutable: trusted,
		GeneratedArgs:     []string{"--dangerously-skip-permissions"},
	}
	session.SetLaunchProgram("claude", sessionenv.AccountLaunchProof{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			session.SetLaunchProgram("claude", sessionenv.AccountLaunchProof{})
			session.SetLaunchProgram(trusted+" --dangerously-skip-permissions", detectedProof)
		}
	}()
	for i := 0; i < 500; i++ {
		program, proof, _, _, _, _, _ := session.launchSnapshot()
		switch program {
		case "claude":
			require.Empty(t, proof.TrustedExecutable)
			require.Empty(t, proof.GeneratedArgs)
		case trusted + " --dangerously-skip-permissions":
			require.Equal(t, detectedProof, proof,
				"a trusted path is provenance for one exact command and must snapshot atomically with it")
		default:
			t.Fatalf("unexpected program %q", program)
		}
	}
	<-done
}

// The plain setter must not leave a previous launch's declaration behind (#3083
// review). SwapAgent installs a handoff target's program and declares nothing, so
// a surviving declaration would vouch for arguments af did not author for the
// replacement — and a target ending in the same instance-specific words would be
// accepted, carrying the account across a handoff meant to fail closed.
func TestSetProgram_ClearsAPreviousDeclaration(t *testing.T) {
	session := &TmuxSession{}
	session.SetLaunchProgram("claude --session-id sid-1 --plugin-dir /p", sessionenv.AccountLaunchProof{
		TrustedExecutable: "/opt/af-detected/claude",
		GeneratedArgs:     []string{"--session-id", "sid-1", "--plugin-dir", "/p"},
	})

	// A handoff target that happens to end in the very same words.
	session.SetProgram("other-agent --session-id sid-1 --plugin-dir /p")

	program, proof, _, _, _, _, _ := session.launchSnapshot()
	require.Equal(t, "other-agent --session-id sid-1 --plugin-dir /p", program)
	require.Empty(t, proof.GeneratedArgs,
		"the outgoing launch's declaration survived into the replacement, so af would vouch for "+
			"arguments it did not author for this command and the account would transfer across the handoff")
	require.Empty(t, proof.TrustedExecutable,
		"an exact executable path is part of the old launch's proof and must not survive replacement")
}
