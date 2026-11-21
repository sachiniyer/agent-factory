package session

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestDockerAccount_ForwardedCountWithoutPairsIsPinnedEmpty pins the fix for
// the count-only forward: GIT_CONFIG_COUNT forwarded and set to N>0 in the
// daemon environment, WITHOUT its required indexed pairs forwarded, would
// ride a bare -e GIT_CONFIG_COUNT into the container while no pair travels
// with it, so every git invocation (including af's provisioning `git config
// --global --add safe.directory "*"`) fails fatally. af neutralizes the
// forward by emitting a trailing -e GIT_CONFIG_COUNT= blank after the bare
// forward: docker's last-wins -e resolves to empty, rendering the whole
// family inert the way an unforwarded count would, and keeping git usable.
//
// The pairs are present in the daemon environment so the test process's own
// git (fixture init, repo-root resolution) survives the count; the incompleteness
// is in the FORWARD (the pairs are not in session_env_passthrough), which is
// what the count resolves against and what the blank guards.
func TestDockerAccount_ForwardedCountWithoutPairsIsPinnedEmpty(t *testing.T) {
	f := newDockerAccountFixture(t, "", "gemini", nil)
	// The operator's claim: GIT_CONFIG_COUNT forwarded AND set to a positive
	// count. The required pairs exist in the daemon environment (so the test
	// process's own git succeeds), but they are NOT in session_env_passthrough,
	// so they do not travel into the container — the count arrives live with
	// zero pairs unless af neutralizes it.
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.directory")
	t.Setenv("GIT_CONFIG_VALUE_0", "/work")
	t.Setenv("GIT_CONFIG_KEY_1", "safe.directory")
	t.Setenv("GIT_CONFIG_VALUE_1", "/other")
	var runArgs []string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runArgs = append([]string(nil), args...)
			return nil, fmt.Errorf("stop after docker run")
		}
		return fakeLocalDockerResponse(args)
	}))

	_, _ = createDockerAccountSession(f, "gemini", []string{"GIT_CONFIG_COUNT"})
	require.NotEmpty(t, runArgs, "the account create never reached docker run")

	// The bare -e GIT_CONFIG_COUNT (forwarded+set) is emitted by
	// DockerForwardNames, and the neutralizing blank follows it (appended by
	// accountLoaderBlankEntries). Docker's last-wins -e resolves to the blank,
	// so git reads count 0 and finds no missing pair. Without the blank, the
	// container would inherit count 2 and every git call would fail with
	// "missing config key GIT_CONFIG_KEY_0".
	bare, found := dockerEnvArg(runArgs, "GIT_CONFIG_COUNT")
	require.True(t, found, "the bare -e GIT_CONFIG_COUNT forward must still be present")
	require.Equal(t, "GIT_CONFIG_COUNT", bare,
		"the count is forwarded+set, so a bare -e rides the daemon value")
	blank, found := dockerEnvArg(runArgs, "GIT_CONFIG_COUNT=")
	require.True(t, found,
		"a forwarded GIT_CONFIG_COUNT without its pairs must be followed by a -e GIT_CONFIG_COUNT= blank so docker's last-wins resolves it empty; runArgs=%v", runArgs)
	require.Equal(t, "GIT_CONFIG_COUNT=", blank,
		"the neutralizing blank must pin the count defined-empty")
	// The pairs are not forwarded, so neither rides a bare -e nor gets a
	// blanking -e — they never reach the container at all, which is safe only
	// because the count is neutralized.
	for _, name := range []string{"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		_, present := dockerEnvArg(runArgs, name)
		require.False(t, present, "%s was never forwarded and must not appear in the container env at all; runArgs=%v", name, runArgs)
	}
}

// The complement of TestDockerAccount_ForwardedCountWithoutPairsIsPinnedEmpty:
// when the operator forwards the count AND every pair it requires — and the
// daemon environment resolves them — the forward is complete, so no
// neutralizing GIT_CONFIG_COUNT= may be appended: the claim rides bare -e
// verbatim. (A forward that is complete only in names but delivers a
// defined-empty KEY is still neutralized — git fails "empty config key" —
// which internal/sessionenv's GitConfigCountForwardMissingPairs tests pin;
// an env-level docker run cannot hold that state because the test process's
// own git would already fail inside fixture setup.)
func TestDockerAccount_ForwardedCompleteCountIsNotNeutralized(t *testing.T) {
	f := newDockerAccountFixture(t, "", "gemini", nil)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.directory")
	t.Setenv("GIT_CONFIG_VALUE_0", "/work")
	var runArgs []string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runArgs = append([]string(nil), args...)
			return nil, fmt.Errorf("stop after docker run")
		}
		return fakeLocalDockerResponse(args)
	}))

	_, _ = createDockerAccountSession(f, "gemini",
		[]string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"})
	require.NotEmpty(t, runArgs, "the account create never reached docker run")

	bare, found := dockerEnvArg(runArgs, "GIT_CONFIG_COUNT")
	require.True(t, found, "the bare -e GIT_CONFIG_COUNT forward must be present")
	require.Equal(t, "GIT_CONFIG_COUNT", bare)
	_, found = dockerEnvArg(runArgs, "GIT_CONFIG_COUNT=")
	require.False(t, found,
		"a complete forward must not be neutralized — no GIT_CONFIG_COUNT= blank; runArgs=%v", runArgs)
}

// TestDockerGitConfigCountNeutralizingNames pins the trailing
// -e GIT_CONFIG_COUNT= at the helper every docker -e assembly shares: a
// passthrough that forwards GIT_CONFIG_COUNT resolving to N>0 in the
// daemon environment without its required pairs arriving usable gets the
// pin, on a plain session exactly as on an account one. A forwarded pair
// that would arrive defined-but-empty counts as missing for a KEY — git
// fails "empty config key" — and as a legal value for a VALUE, which on
// the account path under an agent whose loader blanks unset pair names
// still travels.
func TestDockerGitConfigCountNeutralizingNames(t *testing.T) {
	full := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0=v"}
	all := []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"}

	for _, tt := range []struct {
		name    string
		account string // account agent; "" is a plain session
		pt      []string
		source  []string
		want    bool
	}{
		{"plain count-only forward -> pin", "",
			[]string{"GIT_CONFIG_COUNT"},
			[]string{"GIT_CONFIG_COUNT=1"}, true},
		{"plain forwarded KEY empty in source -> pin", "",
			all,
			[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=", "GIT_CONFIG_VALUE_0=v"}, true},
		{"plain forwarded VALUE unset -> pin (no blanking)", "",
			all,
			[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=k"}, true},
		{"account forwarded KEY unset -> pin (blank delivers KEY=, empty key)", "gemini",
			all,
			[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_VALUE_0=v"}, true},
		{"account forwarded KEY empty in source -> pin", "gemini",
			all,
			[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=", "GIT_CONFIG_VALUE_0=v"}, true},
		{"account forwarded VALUE unset -> no pin (blank delivers legal empty)", "gemini",
			all,
			[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=k"}, false},
		{"complete forward -> no pin", "", all, full, false},
		{"account complete forward -> no pin", "gemini", all, full, false},
		{"count not forwarded -> no pin", "",
			[]string{"GIT_CONFIG_KEY_0"}, full, false},
		{"count absent -> no pin", "", all, []string{"PATH=/bin"}, false},
		{"count git rejects -> no pin (git's diagnostic)", "",
			[]string{"GIT_CONFIG_COUNT"},
			[]string{"GIT_CONFIG_COUNT=2147483648"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &dockerProvisioner{spec: ProvisionSpec{
				Program:               "claude",
				SessionEnvPassthrough: tt.pt,
			}}
			if tt.account != "" {
				p.spec.Account = sessionenv.Account{Agent: tt.account, Name: "work", Dir: "/acct"}
			}
			got := p.gitConfigCountNeutralizingNames(tt.source)
			if tt.want {
				require.Equal(t, []string{"GIT_CONFIG_COUNT="}, got,
					"an incomplete count forward must end the -e assembly with a GIT_CONFIG_COUNT= pin")
			} else {
				require.Empty(t, got)
			}
		})
	}
}

// TestDockerContainerEnv_PlainCountOnlyForwardIsPinnedEmpty is the
// provisioner-level pin for a PLAIN docker session — no account: the bare
// -e GIT_CONFIG_COUNT (forwarded+set) stays, the trailing
// GIT_CONFIG_COUNT= pin follows it so docker's last-wins -e resolves the
// count empty, and the unforwarded pairs never reach the container at all.
// The pairs resolve in the daemon environment so the test process's own
// git stays healthy; the incompleteness is in the forward.
func TestDockerContainerEnv_PlainCountOnlyForwardIsPinnedEmpty(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.directory")
	t.Setenv("GIT_CONFIG_VALUE_0", "/work")
	t.Setenv("GIT_CONFIG_KEY_1", "safe.directory")
	t.Setenv("GIT_CONFIG_VALUE_1", "/other")
	p := &dockerProvisioner{
		program: tmux.ProgramClaude,
		spec:    ProvisionSpec{Program: "claude", SessionEnvPassthrough: []string{"GIT_CONFIG_COUNT"}},
	}
	names := p.containerEnvironmentNames()
	require.Contains(t, names, "GIT_CONFIG_COUNT",
		"the bare -e GIT_CONFIG_COUNT forward must still be present")
	require.Contains(t, names, "GIT_CONFIG_COUNT=",
		"a count-only forward on a plain docker session must end with the GIT_CONFIG_COUNT= pin")
	require.Greater(t,
		slices.Index(names, "GIT_CONFIG_COUNT="), slices.Index(names, "GIT_CONFIG_COUNT"),
		"the pin must follow the bare forward so docker's last-wins resolves it empty")
	for _, name := range []string{"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		require.NotContains(t, names, name,
			"%s was never forwarded and must not reach the container at all", name)
	}
}
