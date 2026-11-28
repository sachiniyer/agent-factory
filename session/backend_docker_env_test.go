package session

import (
	"context"
	"fmt"
	"os"
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
// wrong gitconfig (#7891 R4). It must also mirror docker's VALUE resolution:
// a bare -e/--env name resolves against the environment af hands the docker
// CLI (never the daemon's raw os.Getenv), and a bare name absent there stays
// bare — it cannot clear an earlier =-valued entry (only the built-in
// defaults are removable that way) but still suppresses the image ENV
// fallback, so the last valued entry wins and a name with no valued entry
// is unset (the #7933 review's R4, corrected by the #7949 review's R1). A
// defined-empty result names no global file git reads, so
// agentGitConfigFile reports no target for it (#7949 review R2). These
// tests pin the resolver over the full lexical surface the validators
// already enumerate (dockerForwardedEnvSpellings) plus the bare-name
// matrix, so it cannot drift from docker again. The shared walker's
// positional-stop and value-taking-option swallow (#7726) are already
// pinned by the dockerRunArgsEnvNames tests, which now share the same
// walker.

// agentGitConfigFileTarget resolves the agent's global gitconfig for cases
// expecting a file the agent's git actually reads — a session whose
// container GIT_CONFIG_GLOBAL is defined-empty has no global target at all
// (ok=false), and those cases assert on agentGitConfigFile directly.
func agentGitConfigFileTarget(t *testing.T, p *dockerProvisioner) string {
	t.Helper()
	file, ok := p.agentGitConfigFile()
	require.True(t, ok, "expected a global gitconfig the agent's git reads; got none")
	return file
}

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
			value, state := dockerRunArgsEnvValue(spelling, "GIT_CONFIG_GLOBAL", nil)
			require.Equal(t, dockerRunArgsEnvSet, state,
				"%s sets GIT_CONFIG_GLOBAL in the container", label)
			require.Equal(t, "https://repo.example", value,
				"the resolver must mirror docker: %s sets GIT_CONFIG_GLOBAL in the container", label)
		})
	}
}

// TestDockerRunArgsEnvValue_LastWinsAcrossSpellings pins docker's last-wins
// resolution across the mixed spellings a repo manifest could combine: docker
// keeps the LAST -e for a name whatever its spelling, so the resolver must
// too — including the previously-missed spellings winning over earlier ones.
// The bare -e NAME tail is the engine's name-only mention: the CLI env
// cannot fill it, so it assigns no value, and moby's merge can only remove
// names from the built-in defaults — never an earlier user entry — so the
// earlier =-valued entry stands and agentGitConfigFile follows it (#7949
// review R1).
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
			value, state := dockerRunArgsEnvValue(tt.args, "GIT_CONFIG_GLOBAL", nil)
			require.Equal(t, dockerRunArgsEnvSet, state)
			require.Equal(t, tt.want, value)
		})
	}

	// A bare -e NAME the CLI environment cannot fill changes no value an
	// earlier entry set: moby's ReplaceOrAppendEnvValues removes a bare
	// key only from the built-in defaults (PATH, HOSTNAME, TERM, link
	// env) — appended user entries are never in its removal cache — while
	// the daemon's merge drops the image ENV for any name the user env
	// mentions, and runc keeps the last valued entry (daemon/container/
	// env.go:9, daemon/commit.go:26; #7949 review R1). So the resolver
	// must keep the earlier =-valued entry's value — the "unset clears
	// earlier values" reading the #7933 review proposed was wrong.
	// GIT_CONFIG_GLOBAL never enters the CLI env af hands docker unless
	// the operator forwarded it, so unsetting the host name (restored at
	// cleanup) makes the case independent of the runner's environment.
	if orig, ok := os.LookupEnv("GIT_CONFIG_GLOBAL"); ok {
		t.Cleanup(func() { os.Setenv("GIT_CONFIG_GLOBAL", orig) })
	}
	os.Unsetenv("GIT_CONFIG_GLOBAL")
	p := &dockerProvisioner{
		runArgs: []string{"-e", "GIT_CONFIG_GLOBAL=/runargs.gitconfig", "-e", "GIT_CONFIG_GLOBAL"},
		spec: ProvisionSpec{Program: "codex",
			Account: sessionenv.Account{Dir: "/acct", Agent: "codex", Name: "work"}},
	}
	value, state := dockerRunArgsEnvValue(p.runArgs, "GIT_CONFIG_GLOBAL", p.dockerEnvironment())
	require.Equal(t, dockerRunArgsEnvSet, state,
		"a bare -e the CLI env cannot fill changes no earlier =-valued entry — the merge cannot remove it")
	require.Equal(t, "/runargs.gitconfig", value)
	require.Equal(t, "/runargs.gitconfig", agentGitConfigFileTarget(t, p),
		"the surviving valued entry is the file the agent's git reads")
}

// TestDockerRunArgsEnvEntries_StopsAtOptionTerminator pins the walker's
// pflag contract (#7933 R5): `--` ends docker's option parsing, so no later
// token is a flag — the validators refuse `--` before this walk ever runs
// in production, but the walker's doc says it mirrors pflag, and a consumer
// built on it must not read options past the terminator.
func TestDockerRunArgsEnvEntries_StopsAtOptionTerminator(t *testing.T) {
	args := []string{"--", "-e", "GIT_CONFIG_GLOBAL=/x"}
	value, state := dockerRunArgsEnvValue(args, "GIT_CONFIG_GLOBAL", nil)
	require.Equal(t, dockerRunArgsEnvAbsent, state,
		"an env spelling after -- is a positional word, not an option")
	require.Equal(t, "", value)
	require.Empty(t, dockerRunArgsEnvNames(args),
		"the shared walker's name extractor stops at -- too")
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
			require.Equal(t, "https://repo.example", agentGitConfigFileTarget(t, p),
				"agentGitConfigFile must follow the run_args value docker sets in the container, not the default: %s", label)
			require.NotEqual(t, def, agentGitConfigFileTarget(t, p),
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
			require.Equal(t, "https://repo.example", agentGitConfigFileTarget(t, p),
				"the passthrough-claim arm must resolve the run_args value too: %s", label)
		})
	}
}

// TestAgentGitConfigFile_BareNameResolvesAgainstTheCLIEnv pins the #7924
// follow-up the #7933 review named (R4, corrected by the #7949 review's
// R1/R2): a bare `-e GIT_CONFIG_GLOBAL` or `--env GIT_CONFIG_GLOBAL`
// resolves against the environment af hands the docker CLI
// (dockerEnvironment — DockerCLIEnvironment plus af's own additions),
// never the daemon's raw os.Getenv. docker/cli's ValidateEnv substitutes
// the CLI's own value for a bare name; a name absent there stays bare —
// the daemon's merge drops the image ENV for any name the user env
// mentions, but the bare mention cannot remove an earlier =-valued
// entry — so a bare name with no valued entry resolves to unset and the
// session-home default. A name present in the daemon environment but NOT
// forwarded is equally absent from that CLI env: DockerCLIEnvironment
// admits only client/control names plus the passthrough names the
// operator claimed.
func TestAgentGitConfigFile_BareNameResolvesAgainstTheCLIEnv(t *testing.T) {
	// The image carries GIT_CONFIG_GLOBAL=/image.gitconfig so every case can
	// observe whether the run_args layer correctly dominates or clears the
	// image ENV fallback.
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
			return []byte(`["GIT_CONFIG_GLOBAL=/image.gitconfig"]`), nil
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	}))

	unsetHost := func(t *testing.T) {
		t.Helper()
		if orig, ok := os.LookupEnv("GIT_CONFIG_GLOBAL"); ok {
			t.Cleanup(func() { os.Setenv("GIT_CONFIG_GLOBAL", orig) })
		}
		os.Unsetenv("GIT_CONFIG_GLOBAL")
	}
	account := func(passthrough ...string) *dockerProvisioner {
		return &dockerProvisioner{
			spec: ProvisionSpec{Program: "codex",
				Account:               sessionenv.Account{Dir: "/acct", Agent: "codex", Name: "work"},
				SessionEnvPassthrough: passthrough},
		}
	}
	def := account().sessionHome() + "/.gitconfig"

	// A bare name the CLI env lacks is a name-only mention: with no valued
	// entry the name is unset in the container, and the merge drops the
	// image ENV for it — even though the daemon's own environment sets it.
	// Before the fix the resolver read os.Getenv and returned
	// /host.gitconfig, a value docker never delivers.
	t.Run("daemon env value but not forwarded unsets", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", "/host.gitconfig")
		p := account()
		p.runArgs = []string{"-e", "GIT_CONFIG_GLOBAL"}
		require.Equal(t, def, agentGitConfigFileTarget(t, p),
			"the CLI env carries no unforwarded name — the bare -e resolves to unset, suppressing the image ENV too")
	})

	// Same unset with the name absent from the daemon environment entirely:
	// the image's own ENV must not fill the slot the bare -e vacated.
	t.Run("unexported bare name suppresses the image ENV", func(t *testing.T) {
		unsetHost(t)
		p := account()
		p.runArgs = []string{"-e", "GIT_CONFIG_GLOBAL"}
		require.Equal(t, def, agentGitConfigFileTarget(t, p),
			"a bare -e name the CLI env lacks is unset in the container — the image ENV must not fill it")
	})

	// The bare name resolves to the CLI env's own value when the operator
	// forwarded it: a defined-empty forward resolves to "" exactly as
	// docker's merge keeps NAME= — and since a defined-empty
	// GIT_CONFIG_GLOBAL makes git read NO global file, agentGitConfigFile
	// reports no target rather than a path the agent ignores (#7949 R2).
	t.Run("forwarded defined-empty resolves empty", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", "")
		p := account("GIT_CONFIG_GLOBAL")
		p.runArgs = []string{"-e", "GIT_CONFIG_GLOBAL"}
		value, state := dockerRunArgsEnvValue(p.runArgs, "GIT_CONFIG_GLOBAL", p.dockerEnvironment())
		require.Equal(t, dockerRunArgsEnvSet, state)
		require.Equal(t, "", value,
			"a forwarded defined-empty value resolves to \"\" — the container keeps NAME=, it is not unset")
		file, ok := p.agentGitConfigFile()
		require.False(t, ok,
			"GIT_CONFIG_GLOBAL= names no file the agent's git can read — there is no global target to mark")
		require.Empty(t, file)
	})

	// The positive control: a forwarded name with a host value resolves to
	// that value through the bare -e, the same thing af's own forward emits.
	t.Run("forwarded host value resolves", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", "/operator.gitconfig")
		p := account("GIT_CONFIG_GLOBAL")
		p.runArgs = []string{"-e", "GIT_CONFIG_GLOBAL"}
		require.Equal(t, "/operator.gitconfig", agentGitConfigFileTarget(t, p),
			"a forwarded name is in the CLI env — the bare -e resolves to the operator's value, as docker does")
	})

	// The --env spelling resolves the same way, and `--env=` keeps its own
	// defined-empty semantics rather than being read as a bare name.
	t.Run("--env bare and --env= empty", func(t *testing.T) {
		unsetHost(t)
		p := account()
		p.runArgs = []string{"--env", "GIT_CONFIG_GLOBAL"}
		require.Equal(t, def, agentGitConfigFileTarget(t, p),
			"--env NAME resolves to unset just like -e NAME when the CLI env lacks the name")
		p.runArgs = []string{"--env=GIT_CONFIG_GLOBAL="}
		file, ok := p.agentGitConfigFile()
		require.False(t, ok,
			"--env=GIT_CONFIG_GLOBAL= is a defined-empty pin: git reads no global file at all")
		require.Empty(t, file)
	})

	// R3 (#7949 review): a forwarded-but-host-empty claim pins the name
	// defined-empty even with no run_args entry at all — af's own bare -e
	// forward resolves "" in the CLI env and still suppresses the image
	// ENV — so no global target exists. Reverting the forwarded arm to
	// fall through to the image ENV must fail this.
	t.Run("forwarded host-empty claims the slot over the image ENV", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", "")
		p := account("GIT_CONFIG_GLOBAL")
		file, ok := p.agentGitConfigFile()
		require.False(t, ok,
			"a forwarded host-empty claim keeps GIT_CONFIG_GLOBAL defined-empty — the image ENV must not fill it")
		require.Empty(t, file)
	})
}
