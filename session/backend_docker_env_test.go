package session

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// dockerRunArgsEnvValue is the sole resolver of docker.run_args env values for
// agentGitConfigFile, so it must mirror every spelling the docker engine (and
// af's own validators) honor — otherwise a repo manifest that sets
// GIT_CONFIG_GLOBAL through a missed spelling makes docker point the
// container's GIT_CONFIG_GLOBAL at the run_args file while agentGitConfigFile
// reports the session-home default, and the safe.directory write lands in the
// wrong gitconfig (#7891 R4). These tests pin the resolver over the full
// lexical surface the validators already enumerate (dockerForwardedEnvSpellings)
// so the resolver cannot drift from them again. The shared walker's
// positional-stop and value-taking-option swallow (#7726) are already pinned by
// the dockerRunArgsEnvNames tests, which now share the same walker.

// TestDockerRunArgsEnvValue_ResolvesEveryDockerEffectiveSpelling is the core
// regression: every spelling dockerForwardedEnvSpellings pins for the override
// refusal — the four the old resolver already knew plus the two it missed
// (-e=NAME=v and the combined short-option clusters -te NAME=v / -teNAME=v) —
// must resolve to the value docker would actually set in the container. The
// old resolver returned "" for the four missed-then-fixed spellings, so
// agentGitConfigFile fell through to the session-home default.
func TestDockerRunArgsEnvValue_ResolvesEveryDockerEffectiveSpelling(t *testing.T) {
	for _, spelling := range dockerForwardedEnvSpellings("GIT_CONFIG_GLOBAL") {
		label := strings.Join(spelling, " ")
		t.Run(label, func(t *testing.T) {
			require.Equal(t, "https://repo.example",
				dockerRunArgsEnvValue(spelling, "GIT_CONFIG_GLOBAL"),
				"the resolver must mirror docker: %s sets GIT_CONFIG_GLOBAL in the container", label)
		})
	}
}

// TestDockerRunArgsEnvValue_LastWinsAcrossSpellings pins docker's last-wins
// resolution across the mixed spellings a repo manifest could combine: docker
// keeps the LAST -e for a name whatever its spelling, so the resolver must
// too — including the previously-missed spellings winning over earlier ones.
// A bare -e NAME the host leaves unset resolves to "" and must NOT clobber a
// value an earlier spelling set (the resolver records only a present host
// value for a bare name).
func TestDockerRunArgsEnvValue_LastWinsAcrossSpellings(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missed -e= beats earlier -e",
			args: []string{"-e", "GIT_CONFIG_GLOBAL=/first", "-e=GIT_CONFIG_GLOBAL=/last"},
			want: "/last"},
		{name: "missed cluster-next beats earlier -e",
			args: []string{"-e", "GIT_CONFIG_GLOBAL=/first", "-te", "GIT_CONFIG_GLOBAL=/last"},
			want: "/last"},
		{name: "missed cluster-inline beats earlier -e",
			args: []string{"-e", "GIT_CONFIG_GLOBAL=/first", "-teGIT_CONFIG_GLOBAL=/last"},
			want: "/last"},
		{name: "earlier -e= beaten by later -e",
			args: []string{"-e=GIT_CONFIG_GLOBAL=/first", "-e", "GIT_CONFIG_GLOBAL=/last"},
			want: "/last"},
		{name: "three spellings, last wins",
			args: []string{"-e", "GIT_CONFIG_GLOBAL=/a", "-e=GIT_CONFIG_GLOBAL=/b", "-te", "GIT_CONFIG_GLOBAL=/c"},
			want: "/c"},
		{name: "last-wins is per-name",
			args: []string{"-e", "OTHER=/x", "-e", "GIT_CONFIG_GLOBAL=/g", "-e", "OTHER=/y"},
			want: "/g"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, dockerRunArgsEnvValue(tt.args, "GIT_CONFIG_GLOBAL"))
		})
	}

	// A bare -e NAME the host leaves unset resolves to "" and must not clobber
	// a value an earlier spelling set — the resolver records only a present
	// host value for a bare name.
	require.Equal(t, "/runargs.gitconfig",
		dockerRunArgsEnvValue([]string{"-e", "GIT_CONFIG_GLOBAL=/runargs.gitconfig", "-e", "GIT_CONFIG_GLOBAL"},
			"GIT_CONFIG_GLOBAL"))
}

// TestAgentGitConfigFile_FollowsRunArgsGlobalInEverySpelling is the end-to-end
// fix for the reported bug: on an account session whose repo run_args sets
// GIT_CONFIG_GLOBAL through any docker-effective spelling, agentGitConfigFile
// must return the run_args file (the one the container's git actually reads),
// NOT the session-home default — so trustWorkspaceDirectories writes
// safe.directory into the file the agent's own git consults. The old resolver
// missed -e= and the short-option clusters, so agentGitConfigFile returned the
// default for those four spellings while docker pointed the container
// elsewhere (#7891 R4).
func TestAgentGitConfigFile_FollowsRunArgsGlobalInEverySpelling(t *testing.T) {
	// Stub image inspect to an empty env so the image-ENV fallback arm is
	// inert; the account path is the bug's exposure (GIT_CONFIG_GLOBAL is not
	// a denied name there, and af emits no GIT_CONFIG_GLOBAL re-assert).
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
			return []byte("[]"), nil
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	}))
	def := (&dockerProvisioner{
		spec: ProvisionSpec{Program: "codex",
			Account: sessionenv.Account{Dir: "/acct", Agent: "codex", Name: "work"}},
	}).sessionHome() + "/.gitconfig"

	for _, spelling := range dockerForwardedEnvSpellings("GIT_CONFIG_GLOBAL") {
		label := strings.Join(spelling, " ")
		t.Run("account/"+label, func(t *testing.T) {
			p := &dockerProvisioner{
				runArgs: spelling,
				program: tmux.ProgramCodex,
				spec: ProvisionSpec{Program: "codex",
					Account: sessionenv.Account{Dir: "/acct", Agent: "codex", Name: "work"}},
			}
			require.Equal(t, "https://repo.example", p.agentGitConfigFile(),
				"agentGitConfigFile must follow the run_args value docker sets in the container, not the default: %s", label)
			require.NotEqual(t, def, p.agentGitConfigFile(),
				"the missed spellings used to fall through to the session-home default: %s", label)
		})
	}
}

// TestAgentGitConfigFile_FollowsRunArgsGlobalOnPassthroughPath pins the
// credential-mount path's per-slot claim arm (#7891 N1): a forwarded
// GIT_CONFIG_GLOBAL keeps the lockdown active while the claim stands, so
// agentGitConfigFile must still resolve the operator's run_args value — in
// every spelling, including the ones the old resolver missed.
func TestAgentGitConfigFile_FollowsRunArgsGlobalOnPassthroughPath(t *testing.T) {
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
			return []byte("[]"), nil
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	}))
	for _, spelling := range dockerForwardedEnvSpellings("GIT_CONFIG_GLOBAL") {
		label := strings.Join(spelling, " ")
		t.Run(label, func(t *testing.T) {
			p := &dockerProvisioner{
				runArgs: spelling,
				spec: ProvisionSpec{Program: "gemini",
					SessionEnvPassthrough: []string{"GIT_CONFIG_GLOBAL"}},
				credentialMounts: []string{"mount"},
			}
			require.Equal(t, "https://repo.example", p.agentGitConfigFile(),
				"the passthrough-claim arm must resolve the run_args value too: %s", label)
		})
	}
}
