//go:build !windows

package sessionenv

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression witness for the forgeable env var launch proof: an attacker who
// controls a repo-controlled program_overrides shell string — or any child of
// the agent that can re-invoke af under the marker — can write the proof env
// var themselves with a well-formed base64(JSON) whose TrustedExecutable is
// their own binary. The shim must refuse an agreeing absolute path when the
// env-supplied proof does not match what af's launcher would have produced,
// because the env channel is not evidence af authored this invocation
// (#3123 review, #4731).
//
// The test mirrors the forgeable channel the launcher uses
// (installAccountLaunchProof) for the forged env proof, and installs the
// resolver hook (the same one main.go wires in production) returning the
// legitimate proof a real launcher would have produced for the operator's
// config. The forged proof and the legitimate derivation disagree, so the
// matcher refuses the launch (#3123, #4731 review).
func TestExecProofBypass_RefusesAgreeingAbsolutePathWithForgedProof(t *testing.T) {
	accountDir := t.TempDir()
	installForgedAccountLookup(t, accountDir)
	gotEnviron, execed := installCapturingProcessExec(t)

	// The launcher's intended proof for this pane — the one the operator's
	// resolved config produces, not the one the attacker wrote into the env
	// var. main.go wires the production version; this test wires a minimal one
	// that hands back the trusted claude install the operator selected.
	previousResolver := AccountLaunchProofResolver
	AccountLaunchProofResolver = func(agent, account, command string) (AccountLaunchProof, error) {
		return AccountLaunchProof{TrustedExecutable: "/opt/claude"}, nil
	}
	t.Cleanup(func() { AccountLaunchProofResolver = previousResolver })

	// The attacker supplies their own well-formed proof that names their
	// command as the TrustedExecutable, exactly as the launcher would for the
	// legit install they are impersonating. The forged proof AGREES with the
	// command, so it survives the disagreement guard — the proof-channel match
	// is the only thing standing between them and the account credentials.
	installAccountLaunchProof(t, AccountLaunchProof{TrustedExecutable: "/home/op/repo/claude"})

	require.Equal(t, "claude", AgentForCommand("/home/op/repo/claude"),
		"precondition: an absolute claude path must resolve to the agent, exercising the proof channel rather than disagreement")

	attacker := []string{"claude", "0", "work", "/home/op/repo/claude"}
	err := execInvocation(attacker, true)
	require.Error(t, err, "an agreeing absolute path with an attacker-supplied proof must be refused")
	require.False(t, *execed, "processExec must not run a forged invocation")
	if dir, present := envValue(*gotEnviron, "CLAUDE_CONFIG_DIR"); present {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q leaked into a forged invocation's environment; an attacker-supplied proof must not grant the account scope", dir)
	}
}

// A missing resolver keeps the historical standalone-env behaviour: the
// existing proof-channel regression witnesses (RefusesAgreeingAbsolutePath-
// WithoutProof and the forged full-chain) drive the shim directly without
// wiring main.go, and they must not regress when the resolver is absent.
// Without this guard the env-supplied TrustedExecutable is the only
// authority, and a forged value must still be honored in that path.
func TestExecProofBypass_ForgedProofWithoutResolverFallsThroughForCompatibility(t *testing.T) {
	accountDir := t.TempDir()
	installForgedAccountLookup(t, accountDir)

	previousResolver := AccountLaunchProofResolver
	AccountLaunchProofResolver = nil
	t.Cleanup(func() { AccountLaunchProofResolver = previousResolver })

	installAccountLaunchProof(t, AccountLaunchProof{TrustedExecutable: "/opt/claude"})

	sentinel := errors.New("stop before exec")
	var execed bool
	prev := processExec
	processExec = func(string, []string, []string) error {
		execed = true
		return sentinel
	}
	t.Cleanup(func() { processExec = prev })

	// A resolver-less shim accepting the legit-shaped proof witnesses that the
	// matcher is the only thing the new guard adds: nothing changes about the
	// env-channel authorization when the resolver is unavailable.
	err := execInvocation([]string{"claude", "0", "work", "/opt/claude"}, true)
	require.ErrorIs(t, err, sentinel, "the compat path must still reach exec when the resolver is absent")
	require.True(t, execed, "the compat path must still reach exec when the resolver is absent")
}

// A resolver that returns an error keeps the historical standalone-env
// behaviour for production paths where re-derivation cannot decide (e.g. the
// pane runs from a context no config exists for). The fallback is the env
// proof alone, so the regression is silent rather than a broad refusal: the
// resolver result is a SECONDARY gate, not a replacement for the env channel.
func TestExecProofBypass_ResolverErrorFallsBackToEnvProof(t *testing.T) {
	accountDir := t.TempDir()
	installForgedAccountLookup(t, accountDir)

	previousResolver := AccountLaunchProofResolver
	AccountLaunchProofResolver = func(string, string, string) (AccountLaunchProof, error) {
		return AccountLaunchProof{}, errors.New("config not reachable from this pane")
	}
	t.Cleanup(func() { AccountLaunchProofResolver = previousResolver })

	installAccountLaunchProof(t, AccountLaunchProof{TrustedExecutable: "/opt/claude"})

	sentinel := errors.New("stop before exec")
	var execed bool
	prev := processExec
	processExec = func(string, []string, []string) error {
		execed = true
		return sentinel
	}
	t.Cleanup(func() { processExec = prev })

	err := execInvocation([]string{"claude", "0", "work", "/opt/claude"}, true)
	require.ErrorIs(t, err, sentinel, "an unresolvable derivation must fall back to the env proof the launcher installed")
	require.True(t, execed, "the resolver-error fallback must still reach exec")
}
