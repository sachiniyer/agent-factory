//go:build !windows

package sessionenv

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression witnesses for the forgeable-argv TrustedExecutable bypass:
// a repository-controlled program_overrides value can re-invoke af under
// AccountExecMarker and supply its own argv, but it cannot supply the
// launcher's out-of-band proof (it runs in an UNSCOPED outer pane whose
// environment never received it) and it cannot make a non-agent command
// resolve to the selected agent. The shim must refuse the forged invocation
// rather than grant the account credential directory to an attacker
// executable — the #3051 fail-closed property, applied to provenance.
//
// These tests drive the real execInvocation -> execInvocationMode(scoped=true)
// path against a real AccountLookup, and assert processExec is never reached
// and the account credential directory ("CLAUDE_CONFIG_DIR") is never injected.

func installForgedAccountLookup(t *testing.T, accountDir string) {
	t.Helper()
	prev := AccountLookup
	AccountLookup = func(agent, name string) (Account, error) {
		if agent != "claude" || name != "work" {
			return Account{}, errors.New("no such account")
		}
		// TrustedExecutable is intentionally NOT set on the stored account: in the
		// forged path the only source of TrustedExecutable is what the attacker
		// supplies, and the fix must refuse to honor that.
		return Account{Agent: "claude", Name: "work", Dir: accountDir}, nil
	}
	t.Cleanup(func() { AccountLookup = prev })
}

func installCapturingProcessExec(t *testing.T) (*[]string, *bool) {
	t.Helper()
	var gotEnviron []string
	var execed bool
	prev := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		execed = true
		gotEnviron = append([]string(nil), environ...)
		return errors.New("must not reach exec")
	}
	t.Cleanup(func() { processExec = prev })
	return &gotEnviron, &execed
}

// The documented exploit's exact argv (forged TrustedExecutable == command ==
// an attacker executable). Before the fix this reached processExec with
// CLAUDE_CONFIG_DIR=<account dir> and the ambient key stripped (the bug). After
// the fix the forged argv is refused: no proof env var was set by a launcher,
// and the command is not the selected agent.
func TestExecProofBypass_FullChainForgedAccountExecMarker(t *testing.T) {
	accountDir := t.TempDir()
	installForgedAccountLookup(t, accountDir)
	gotEnviron, execed := installCapturingProcessExec(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-must-not-survive")
	t.Setenv(accountLaunchProofEnvVar, "") // no launcher set the proof

	// New marker argv shape: <agent> <extras-count> <account> <command>.
	attacker := []string{"claude", "0", "work", "./.evil"}
	err := execInvocation(attacker, true)
	require.Error(t, err, "the forged argv must be refused, not granted the account scope")
	require.False(t, *execed, "processExec must not run a forged invocation")
	if dir, present := envValue(*gotEnviron, "CLAUDE_CONFIG_DIR"); present {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q leaked into a forged invocation's environment; the account directory must never reach an attacker executable", dir)
	}
}

// An attacker who names their binary `claude` and reaches it by absolute path
// ($PWD/claude, expanded by the outer /bin/sh) resolves to the selected agent
// (AgentForCommand basenames `/abs/claude` to `claude`), so the disagreement
// guard does not fire — but the out-of-band proof is still absent, so the
// scope is refused on the missing-proof gate. This is the case the
// naive disagreement-only fix would miss and the proof channel closes.
func TestExecProofBypass_RefusesAgreeingAbsolutePathWithoutProof(t *testing.T) {
	accountDir := t.TempDir()
	installForgedAccountLookup(t, accountDir)
	gotEnviron, execed := installCapturingProcessExec(t)
	t.Setenv(accountLaunchProofEnvVar, "") // no launcher set the proof

	// An absolute path that basenames to `claude` so AgentForCommand AGREES.
	require.Equal(t, "claude", AgentForCommand("/home/op/repo/claude"),
		"precondition: an absolute claude path must resolve to the agent, exercising the proof gate rather than disagreement")
	attacker := []string{"claude", "0", "work", "/home/op/repo/claude"}
	err := execInvocation(attacker, true)
	require.Error(t, err, "an agreeing command without the launcher's proof must be refused")
	require.False(t, *execed, "processExec must not run a proof-less agreeing invocation")
	if dir, present := envValue(*gotEnviron, "CLAUDE_CONFIG_DIR"); present {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q leaked without a Launcher proof", dir)
	}
}

// The legitimate end-to-end path still works: a launcher-installed proof,
// a command that resolves to the selected agent, and a real account dir. This
// guards against the fix over-refusing (regression for the absolute-path
// install the TrustedExecutable feature exists to support).
func TestExecProofBypass_LegitimateAbsolutePathLaunchIsScoped(t *testing.T) {
	accountDir := t.TempDir()
	installForgedAccountLookup(t, accountDir)
	var gotEnviron []string
	sentinel := errors.New("stop before exec")
	prev := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnviron = append([]string(nil), environ...)
		return sentinel
	}
	t.Cleanup(func() { processExec = prev })
	t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-must-not-survive")
	installAccountLaunchProof(t, AccountLaunchProof{TrustedExecutable: "/opt/claude"})

	err := execInvocation([]string{"claude", "0", "work", "/opt/claude"}, true)
	require.ErrorIs(t, err, sentinel, "the legitimate proof-bearing launch must reach exec")
	dir, present := envValue(gotEnviron, "CLAUDE_CONFIG_DIR")
	require.True(t, present, "the legitimate launch must inject the account credential root")
	require.Equal(t, accountDir, dir)
	_, leaked := envValue(gotEnviron, "ANTHROPIC_API_KEY")
	require.False(t, leaked, "the ambient key must be stripped from a scoped launch")
	_, proofLeaked := envValue(gotEnviron, accountLaunchProofEnvVar)
	require.False(t, proofLeaked, "the proof variable must not leak to the agent")
}
