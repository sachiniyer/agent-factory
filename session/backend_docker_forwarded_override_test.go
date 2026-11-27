package session

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// The forwarded-name override (#7867 R2): a routing or unclassified name the
// operator forwards through session_env_passthrough could be overridden by a
// repo `-e` in docker.run_args, because af emits the forwarded names as bare
// `-e NAME` entries ahead of run_args and Docker keeps the LAST -e for a name.
// Both guarded validators now refuse a run_args env entry that names any name
// in the session's effective passthrough, whatever its class.

// dockerForwardedEnvSpellings enumerates every `docker run` spelling that sets
// an environment name, so the override refusal is pinned against the whole
// lexical surface — the direct forms, the attached and cluster shorthands, and
// the env-file escape. Each would land the repo value after af's forwarded
// entries in argv order.
func dockerForwardedEnvSpellings(name string) [][]string {
	return [][]string{
		{"-e", name + "=https://repo.example"},
		{"--env", name + "=https://repo.example"},
		{"--env=" + name + "=https://repo.example"},
		{"-e" + name + "=https://repo.example"},
		{"-e", name}, // the bare spelling claims the name the operator claims
		{"-e=" + name + "=https://repo.example"},
		{"-te", name + "=https://repo.example"},
		{"-ite", name + "=https://repo.example"},
		{"-te" + name + "=https://repo.example"},
	}
}

// TestDockerRunArgs_EnvFileNamingForwardedNameStaysRefused pins the --env-file
// half of R2's spelling coverage: an env file naming a forwarded name is
// refused the same way one naming anything else is — af cannot prove its
// contents, so the file never reaches the point where its entries could
// override a forwarded value.
func TestDockerRunArgs_EnvFileNamingForwardedNameStaysRefused(t *testing.T) {
	for _, spelling := range [][]string{{"--env-file", "repo.env"}, {"--env-file=repo.env"}} {
		label := strings.Join(spelling, " ")
		t.Run("account/"+label, func(t *testing.T) {
			err := validateAccountDockerRunArgs(spelling, "codex", []string{"OPENAI_BASE_URL"})
			require.Error(t, err)
			require.Contains(t, err.Error(), "--env-file")
		})
		t.Run("credential/"+label, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(spelling, tmux.ProgramCodex, []string{"OPENAI_BASE_URL"})
			require.Error(t, err)
			require.Contains(t, err.Error(), "--env-file")
		})
	}
}

// TestDockerRunArgs_RefusesForwardedNameOverride is the unit half of R2: a
// forwarded name in every spelling is refused on BOTH docker paths, and the
// refusal names the entry plus the mechanism — the operator's forwarded value
// is what a repo -e would override.
func TestDockerRunArgs_RefusesForwardedNameOverride(t *testing.T) {
	passthrough := []string{"OPENAI_BASE_URL"}
	for _, spelling := range dockerForwardedEnvSpellings("OPENAI_BASE_URL") {
		label := strings.Join(spelling, " ")
		t.Run("account/"+label, func(t *testing.T) {
			err := validateAccountDockerRunArgs(spelling, "codex", passthrough)
			require.Error(t, err, "a run_args env entry for a forwarded name must be refused")
			require.Contains(t, err.Error(), "OPENAI_BASE_URL")
			require.Contains(t, err.Error(), "session_env_passthrough")
			require.Contains(t, err.Error(), "override")
		})
		t.Run("credential/"+label, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(spelling, tmux.ProgramCodex, passthrough)
			require.Error(t, err, "a run_args env entry for a forwarded name must be refused")
			require.Contains(t, err.Error(), "OPENAI_BASE_URL")
			require.Contains(t, err.Error(), "session_env_passthrough")
			require.Contains(t, err.Error(), "override")
		})
	}
}

// TestDockerRunArgs_ForwardedOverrideCoversEveryClass pins the "whatever its
// class" half of R2: an unclassified forwarded name and a classified routing
// name the operator forwards are refused alike — the refusal reports the
// override mechanism rather than the name's class, because "forward it through
// session_env_passthrough" is a remedy the operator already took.
func TestDockerRunArgs_ForwardedOverrideCoversEveryClass(t *testing.T) {
	for _, tt := range []struct {
		name        string
		passthrough []string
	}{
		{"unclassified", []string{"MY_REPO_TOOL_URL"}},
		{"routing", []string{"HTTPS_PROXY"}},
		{"endpoint", []string{"ANTHROPIC_BASE_URL"}},
	} {
		forwarded := tt.passthrough[0]
		t.Run(tt.name+"/account", func(t *testing.T) {
			err := validateAccountDockerRunArgs([]string{"-e", forwarded + "=https://repo.example"}, "claude", tt.passthrough)
			require.Error(t, err)
			require.Contains(t, err.Error(), forwarded)
			require.Contains(t, err.Error(), "override")
			require.NotContains(t, err.Error(), "forward it through session_env_passthrough",
				"the override refusal must not re-offer the channel the operator already used")
		})
		t.Run(tt.name+"/credential", func(t *testing.T) {
			err := validateCredentialDockerRunArgs([]string{"-e", forwarded + "=https://repo.example"}, tmux.ProgramClaude, tt.passthrough)
			require.Error(t, err)
			require.Contains(t, err.Error(), forwarded)
			require.Contains(t, err.Error(), "override")
		})
	}
}

// TestDockerRunArgs_ForwardedOverrideKeepsUnclaimedNames pins the boundary:
// names the operator did NOT forward keep their existing treatment — an
// unclassified name stays accepted, a routing name keeps its classified
// refusal on the account path, and nothing about the override check changes
// the posture for names the operator never claimed.
func TestDockerRunArgs_ForwardedOverrideKeepsUnclaimedNames(t *testing.T) {
	require.NoError(t, validateAccountDockerRunArgs([]string{"-e", "MY_REPO_TOOL_URL=x"}, "codex", nil),
		"an unclassified name the operator never forwarded stays accepted")
	require.NoError(t, validateCredentialDockerRunArgs([]string{"-e", "MY_REPO_TOOL_URL=x"}, tmux.ProgramCodex, nil),
		"an unclassified name the operator never forwarded stays accepted")
	require.NoError(t, validateAccountDockerRunArgs([]string{"-e", "OTHER_NAME=x"}, "codex", []string{"OPENAI_BASE_URL"}),
		"a forwarded OTHER name does not taint unclaimed entries")

	// An unforwarded routing name keeps its classified refusal and remedy —
	// the override check must not swallow it.
	err := validateAccountDockerRunArgs([]string{"-e", "HTTPS_PROXY=https://repo.example"}, "codex", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "session_env_passthrough")
	require.Contains(t, err.Error(), "credential")
}

// TestDockerRunArgs_PaneBoundaryNamesKeepClassifiedRefusal pins the
// pane-boundary carve-out: forwarding TMUX is itself refused at Provision, so
// it can never carry an effective claim — a repo -e naming it keeps the
// classified refusal rather than the override message.
func TestDockerRunArgs_PaneBoundaryNamesKeepClassifiedRefusal(t *testing.T) {
	// The credential-mount path classifies pane-boundary names in
	// credentialMountDeniedReason, so -e TMUX=x is refused with that reason —
	// never the override message, since no forwarded value exists to override.
	err := validateCredentialDockerRunArgs([]string{"-e", "TMUX=/tmp/x"}, tmux.ProgramCodex, []string{"TMUX"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "override")

	// The account path has no class for TMUX — af's blanking -e dominates a
	// repo entry — so it stays accepted even while listed: the passthrough
	// entry is refused later at Provision for the same reason the blank exists.
	require.NoError(t, validateAccountDockerRunArgs([]string{"-e", "TMUX=/tmp/x"}, "codex", []string{"TMUX"}))
}

// TestDockerAccountRunArgs_RefusesForwardedNameOverride is the account-side
// end-to-end fix: a forwarded OPENAI_BASE_URL plus a repo -e naming it is
// refused BEFORE docker run, so the container never starts on an override.
func TestDockerAccountRunArgs_RefusesForwardedNameOverride(t *testing.T) {
	for _, spelling := range dockerForwardedEnvSpellings("OPENAI_BASE_URL") {
		t.Run(strings.Join(spelling, " "), func(t *testing.T) {
			runCalled, err, _ := provisionDockerAccountImageEnv(t, "codex", spelling, []string{"OPENAI_BASE_URL"}, nil)
			require.Error(t, err, "a run_args env entry for a forwarded name must be refused")
			require.Contains(t, err.Error(), "OPENAI_BASE_URL")
			require.False(t, runCalled, "the refusal must happen before docker run")
		})
	}
}

// TestDockerCredentialRunArgs_RefusesForwardedNameOverride is the
// credential-mount-side end-to-end fix, the symmetric refusal on the path the
// grant enables.
func TestDockerCredentialRunArgs_RefusesForwardedNameOverride(t *testing.T) {
	for _, spelling := range dockerForwardedEnvSpellings("OPENAI_BASE_URL") {
		t.Run(strings.Join(spelling, " "), func(t *testing.T) {
			runCalled, err, _ := provisionDockerCredentialGrantPassthrough(t, "codex", spelling, []string{"OPENAI_BASE_URL"})
			require.Error(t, err, "a run_args env entry for a forwarded name must be refused")
			require.Contains(t, err.Error(), "OPENAI_BASE_URL")
			require.False(t, runCalled, "the refusal must happen before docker run")
		})
	}
}
