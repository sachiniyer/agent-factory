//go:build linux || darwin

package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// TestDockerCredentialRunArgs_RefusesProgramChoiceNames is the run_args half
// of #7529 R7, end-to-end: PATH, LD_PRELOAD and LD_LIBRARY_PATH let a repo
// choose which program af's in-container steps run (a PATH that resolves its
// committed .x/sh for every `sh -c`) or which library every dynamically
// linked program loads. Each is refused before `docker run` — the refusal is
// what makes the image-PATH re-assertion and the absolute /bin/sh safe to
// rely on — and the error names the variable and its mechanism.
func TestDockerCredentialRunArgs_RefusesProgramChoiceNames(t *testing.T) {
	for _, name := range []string{"PATH", "LD_PRELOAD", "LD_LIBRARY_PATH"} {
		for _, spelling := range [][]string{
			{"-e", name + "=/workspace/.x/bin"},
			{"-e", name}, // the bare spelling resolves through the CLI env the same way
			{"--env=" + name + "=/workspace/.x"},
		} {
			t.Run(name+"/"+strings.Join(spelling, " "), func(t *testing.T) {
				runCalled, err, _ := provisionDockerCredentialGrant(t, tmux.ProgramClaude, spelling)
				require.Error(t, err, "a run_args %s entry must be refused", name)
				require.Contains(t, err.Error(), name)
				require.False(t, runCalled, "a program-choice run_arg must refuse before docker run")
			})
		}
	}
}

// TestDockerCredentialRunArgs_ProgramChoiceReasonNamesTheMechanism pins the
// refusal's because-clause: PATH's names the program-choosing mechanism (and
// the re-assertion remedy), the loader names' names library injection — the
// operator reading the refusal must be told why, not just what.
func TestDockerCredentialRunArgs_ProgramChoiceReasonNamesTheMechanism(t *testing.T) {
	err := validateCredentialDockerRunArgs([]string{"-e", "PATH=/workspace/.x"}, tmux.ProgramClaude)
	require.Error(t, err)
	require.Contains(t, err.Error(), "which program", "PATH's refusal must name the program-choosing mechanism")

	for _, name := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH"} {
		err := validateCredentialDockerRunArgs([]string{"-e", name + "=/workspace/.x/x.so"}, tmux.ProgramClaude)
		require.Error(t, err)
		require.Contains(t, err.Error(), "library", "%s's refusal must name the library-injection mechanism", name)
	}
}

// provisionDockerCredentialGrantImageEnv is provisionDockerCredentialGrant-
// Passthrough plus a docker image inspect answer: it returns imageEnv as the
// image's Config.Env so the PATH re-assertion has a source to read.
func provisionDockerCredentialGrantImageEnv(t *testing.T, program string, runArgs, passthrough, imageEnv []string) (runCalled bool, err error, runArgv []string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if files, ok := agentCredentialFiles[program]; ok && len(files) > 0 {
		writeCredFile(t, filepath.Join(home, files[0]))
	}
	_ = forceCredentialMountMode(t)
	cfg := config.DefaultConfig()
	cfg.DockerMountAgentCredentials = true
	require.NoError(t, config.SaveConfig(cfg))

	repoRoot := initTempGitRepo(t)
	dockerCfg := map[string]any{"image": "example.invalid/agent:latest"}
	if runArgs != nil {
		dockerCfg["run_args"] = runArgs
	}
	writeInRepoConfig(t, repoRoot, map[string]any{"backend": "docker", "docker": dockerCfg})

	envJSON := "[" + strings.Join(quoteEnvEntries(imageEnv), ",") + "]"
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	t.Cleanup(SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af")))
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte("credential-test-engine\n"), nil
		}
		if len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
			return []byte(envJSON), nil
		}
		if len(args) > 0 && args[0] == "run" {
			runCalled = true
			runArgv = append([]string(nil), args...)
		}
		return nil, fmt.Errorf("stop after docker run")
	}))

	_, err = (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot:              repoRoot,
		Title:                 "credential-path-reassert",
		Program:               program,
		CloneURL:              "file:///fixture.git",
		SessionEnvPassthrough: passthrough,
	})
	return runCalled, err, runArgv
}

func quoteEnvEntries(entries []string) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = `"` + strings.ReplaceAll(e, `"`, `\"`) + `"`
	}
	return out
}

// TestDockerMountAgentCredentials_PATHReassertedFromImageEnv pins the
// re-assertion half of #7529 R7: on the credential-mount path the container's
// PATH is re-asserted to the image's OWN Config.Env PATH, emitted after
// run_args so Docker's last-wins rule makes it dominate any earlier PATH a
// repo source smuggled past the lexical guard. It is a valued re-assertion,
// never a defined-empty pin (a blank PATH would break every exec), and it is
// not applied when the operator forwards their own PATH.
func TestDockerMountAgentCredentials_PATHReassertedFromImageEnv(t *testing.T) {
	imageEnv := []string{"PATH=/img/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
	runCalled, _, runArgv := provisionDockerCredentialGrantImageEnv(t, tmux.ProgramClaude,
		[]string{"-e", "TZ=UTC"}, nil, imageEnv)
	require.True(t, runCalled, "harmless run_args must still reach docker run")

	reassertIdx := indexIn(runArgv, "PATH=/img/bin:/usr/bin:/bin")
	runArgsIdx := indexIn(runArgv, "TZ=UTC")
	require.Greaterf(t, reassertIdx, 0,
		"the image's own PATH was not re-asserted in the run argv: %v", runArgv)
	require.Greaterf(t, reassertIdx, runArgsIdx,
		"the PATH re-assertion must come AFTER run_args so Docker's last-wins rule makes it dominate: %v", runArgv)
	require.Equal(t, -1, envPinIndex(runArgv, "PATH"),
		"PATH must be re-asserted with the image's value, never pinned empty: %v", runArgv)
}

// TestDockerMountAgentCredentials_PATHReassertSkippedWhenForwarded pins the
// operator exemption: PATH named in session_env_passthrough is the operator's
// own claim on the name, so no image-PATH re-assertion is emitted to dominate
// it — the bare `-e PATH` the forwarder emits is the only PATH entry.
func TestDockerMountAgentCredentials_PATHReassertSkippedWhenForwarded(t *testing.T) {
	imageEnv := []string{"PATH=/img/bin:/usr/bin:/bin"}
	runCalled, _, runArgv := provisionDockerCredentialGrantImageEnv(t, tmux.ProgramClaude,
		[]string{"-e", "TZ=UTC"}, []string{"PATH"}, imageEnv)
	require.True(t, runCalled)
	require.Equal(t, -1, indexIn(runArgv, "PATH=/img/bin:/usr/bin:/bin"),
		"an operator-forwarded PATH must keep the operator's claim — no re-assertion: %v", runArgv)
}

// TestDockerMountAgentCredentials_PATHReassertSkippedWhenUninspectable pins
// the best-effort posture: when the image cannot be inspected (not pulled
// yet), no re-assertion is emitted — with run_args PATH refused, the
// container then inherits the image's own PATH anyway, which is the same
// trusted source the re-assertion would have named.
func TestDockerMountAgentCredentials_PATHReassertSkippedWhenUninspectable(t *testing.T) {
	// The shared harness's fake answers no `image inspect`, so dockerImageEnv
	// returns nil — the re-assertion must simply not appear, and provisioning
	// must still reach `docker run`.
	runCalled, _, runArgv := provisionDockerCredentialGrant(t, tmux.ProgramClaude,
		[]string{"-e", "TZ=UTC"})
	require.True(t, runCalled)
	for _, arg := range runArgv {
		require.Falsef(t, strings.HasPrefix(arg, "PATH="),
			"an unreadable image PATH must not be re-asserted: %v", runArgv)
	}
}

// TestDockerExecProgramsUseAbsoluteShell is the absolute-path half of #7529
// R7: every `docker exec` af issues names its program absolutely — /bin/sh —
// so a repo-shaped container PATH can never hand af's own steps a program of
// the repo's choosing (a committed .x/sh intercepting the agent-server
// launch, pins included). Script-internal commands (git, cat, chmod) still
// resolve through PATH — the image's own, re-asserted above.
func TestDockerExecProgramsUseAbsoluteShell(t *testing.T) {
	repo := dockerNonAccountProvisionFixture(t, "nonroot/image:latest")
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(fakeNonRootImageDockerAnswers(t, &calls, dockerNonRootImageUser)))

	_, err := dockerRuntime{}.Provision(ProvisionSpec{
		RepoRoot: repo,
		Title:    "absolute-exec-programs",
		CloneURL: "file:///fixture.git",
	})
	require.NoError(t, err, "a non-account session must provision; calls=%v", calls)

	var execCalls int
	for _, call := range calls {
		if len(call) == 0 || call[0] != "exec" {
			continue
		}
		execCalls++
		idIdx := indexIn(call, dockerCreatedID)
		require.Greaterf(t, idIdx, 0, "exec call missing the container id: %v", call)
		require.Lessf(t, idIdx+1, len(call), "exec call names no program after the container id: %v", call)
		require.Containsf(t,
			append([]string{dockerContainerShell, dockerContainerCat}, dockerContainerStatPaths...),
			call[idIdx+1],
			"the exec's program must be an absolute path, never a PATH-resolved name: %v", call)
	}
	require.Greater(t, execCalls, 0, "the fixture must drive at least one docker exec")
}

// TestDockerContainerEnvironment_NonMountSessionsMatchBaseline is the #7529
// R8 regression pin: the proxy-twin normalization is part of the
// credential-mount pins' contract, so on a session with NO credential mounts
// — account or plain — the container env names and the docker CLI environment
// are exactly what the shared helpers produce (the pre-normalization shape).
// A forwarded proxy name must not pull a case twin into argv or synthesize a
// value on those paths.
func TestDockerContainerEnvironment_NonMountSessionsMatchBaseline(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://operator-proxy:8080")
	passthrough := []string{"HTTPS_PROXY"}

	for _, tt := range []struct {
		name string
		p    *dockerProvisioner
	}{
		{"plain", &dockerProvisioner{program: tmux.ProgramClaude, spec: ProvisionSpec{Program: "claude", SessionEnvPassthrough: passthrough}}},
		{"account", &dockerProvisioner{program: tmux.ProgramClaude, spec: ProvisionSpec{
			Program:               "claude",
			SessionEnvPassthrough: passthrough,
			Account:               sessionenv.Account{Dir: "/tmp/acct"},
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			agent := tt.p.agentName()
			wantNames := sessionenv.DockerForwardNames(os.Environ(), agent, passthrough)
			require.Equal(t, wantNames, tt.p.containerEnvironmentNames(),
				"a session with no credential mounts must emit exactly the helper's env names")
			wantEnv := sessionenv.DockerCLIEnvironment(os.Environ(), agent, passthrough)
			require.Equal(t, wantEnv, tt.p.dockerEnvironment(),
				"a session with no credential mounts must emit exactly the helper's CLI environment")
			require.NotContains(t, tt.p.containerEnvironmentNames(), "https_proxy",
				"no case twin may be emitted without a credential mount")
		})
	}
}

// TestDockerContainerEnvironment_MountSessionNormalizesTwin is the other arm
// of the R8 pin: on the credential-mount path itself the gate is open — the
// unforwarded twin still joins the env names and the synthesized value still
// joins the CLI environment, so the normalization's gate is on mounts, not
// off entirely.
func TestDockerContainerEnvironment_MountSessionNormalizesTwin(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://operator-proxy:8080")
	passthrough := []string{"HTTPS_PROXY"}
	p := &dockerProvisioner{
		program:          tmux.ProgramClaude,
		credentialMounts: []string{"-v", "/h/cred:/c/cred:ro"},
		spec:             ProvisionSpec{Program: "claude", SessionEnvPassthrough: passthrough},
	}
	require.Contains(t, p.containerEnvironmentNames(), "https_proxy",
		"the unforwarded twin must be emitted on the credential-mount path")
	require.Contains(t, p.dockerEnvironment(), "https_proxy=http://operator-proxy:8080",
		"the twin must resolve to the operator's value through the CLI environment")
	for _, arg := range p.containerEnvironmentNames() {
		require.NotContains(t, arg, "operator-proxy",
			"the proxy value must never enter argv — only the name does")
	}
}

// TestCredentialMountPinnedNames_ProgramChoiceNamesNotPinned pins the
// refuse-vs-pin split: PATH and the loader names are refused to repo
// run_args, but PATH is re-asserted (never blanked) and the loader names are
// left at the image's own values (the same trust class as its binaries) — a
// defined-empty PATH or LD_LIBRARY_PATH would break the container outright.
func TestCredentialMountPinnedNames_ProgramChoiceNamesNotPinned(t *testing.T) {
	pinned := sortedNamesToSet(credentialMountPinnedNames(tmux.ProgramClaude, nil))
	denied := credentialMountDeniedNames(tmux.ProgramClaude)
	for _, name := range []string{"PATH", "LD_PRELOAD", "LD_LIBRARY_PATH"} {
		require.Contains(t, denied, name, "%s must be refused to run_args", name)
		require.NotContains(t, pinned, name, "%s must not be pinned defined-empty", name)
	}
}

// provisionDockerAccountImageEnv provisions an account session far enough to
// capture the `docker run` argv, answering `image inspect` with imageEnv as
// the image's Config.Env so the account PATH re-assertion has a source to
// read — provisionDockerCredentialGrantImageEnv's account-path mirror (#7563
// R2).
func provisionDockerAccountImageEnv(t *testing.T, program string, runArgs, passthrough, imageEnv []string) (runCalled bool, err error, runArgv []string) {
	t.Helper()
	f := newDockerAccountFixture(t, "", program, runArgs)
	envJSON := "[" + strings.Join(quoteEnvEntries(imageEnv), ",") + "]"
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
			return []byte(envJSON), nil
		}
		if len(args) > 0 && args[0] == "run" {
			runCalled = true
			runArgv = append([]string(nil), args...)
			return nil, fmt.Errorf("stop after docker run")
		}
		return fakeLocalDockerResponse(args)
	}))
	_, err = createDockerAccountSession(f, program, passthrough)
	return runCalled, err, runArgv
}

// TestDockerAccountRunArgs_RefusesProgramChoiceNames is the account-side half
// of #7563 R2, the #7529 R7 refusal extended to account sessions: PATH and
// the loader names let a repo choose which program — or which loaded code —
// af's account provisioning execs run (a committed .x/cat answering the
// mount-table probe, or .x/chown), so each is refused before `docker run`
// and the error names the variable.
func TestDockerAccountRunArgs_RefusesProgramChoiceNames(t *testing.T) {
	for _, name := range []string{"PATH", "LD_PRELOAD", "LD_LIBRARY_PATH"} {
		for _, spelling := range [][]string{
			{"-e", name + "=/workspace/.x/bin"},
			{"-e", name}, // the bare spelling resolves through the CLI env the same way
			{"--env=" + name + "=/workspace/.x"},
		} {
			t.Run(name+"/"+strings.Join(spelling, " "), func(t *testing.T) {
				runCalled, err, _ := provisionDockerAccountImageEnv(t, "codex", spelling, nil, nil)
				require.Error(t, err, "a run_args %s entry must be refused", name)
				require.Contains(t, err.Error(), name)
				require.False(t, runCalled, "a program-choice run_arg must refuse before docker run")
			})
		}
	}
}

// TestDockerAccountRunArgs_ProgramChoiceReasonNamesTheMechanism pins the
// refusal's because-clause on the account path: PATH names the
// program-choosing mechanism and the re-assertion remedy; the loader names
// name library injection — the operator reading the refusal must be told
// why, not just what.
func TestDockerAccountRunArgs_ProgramChoiceReasonNamesTheMechanism(t *testing.T) {
	err := validateAccountDockerRunArgs([]string{"-e", "PATH=/workspace/.x"}, "codex")
	require.Error(t, err)
	require.Contains(t, err.Error(), "which program", "PATH's refusal must name the program-choosing mechanism")
	// #7571 review R9: the refusal must name the remedy too — the image's
	// ENV can carry the value the repo wanted, or the operator can forward
	// their own through session_env_passthrough.
	require.Contains(t, err.Error(), "image's ENV", "PATH's refusal must name the image-ENV remedy")
	require.Contains(t, err.Error(), "session_env_passthrough", "PATH's refusal must name the operator's passthrough remedy")

	for _, name := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH"} {
		err := validateAccountDockerRunArgs([]string{"-e", name + "=/workspace/.x/x.so"}, "codex")
		require.Error(t, err)
		require.Contains(t, err.Error(), "library", "%s's refusal must name the library-injection mechanism", name)
		require.Contains(t, err.Error(), "session_env_passthrough", "%s's refusal must name the remedy too", name)
	}
}

// TestDockerAccount_PATHReassertedFromImageEnv pins the re-assertion half of
// #7563 R2 on the account path: the container's PATH is re-asserted to the
// image's OWN Config.Env PATH, emitted after run_args so Docker's last-wins
// rule makes it dominate any earlier PATH — the same guarantee #7529 R7 gave
// credential-mount sessions. No repo-chosen PATH entry can then resolve a
// program af runs: the mount-table probe, the chown, or the agent's bare
// executable.
func TestDockerAccount_PATHReassertedFromImageEnv(t *testing.T) {
	imageEnv := []string{"PATH=/img/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
	runCalled, _, runArgv := provisionDockerAccountImageEnv(t, "codex",
		[]string{"-e", "TZ=UTC"}, nil, imageEnv)
	require.True(t, runCalled, "harmless run_args must still reach docker run")

	reassertIdx := indexIn(runArgv, "PATH=/img/bin:/usr/bin:/bin")
	runArgsIdx := indexIn(runArgv, "TZ=UTC")
	require.Greaterf(t, reassertIdx, 0,
		"the image's own PATH was not re-asserted in the run argv: %v", runArgv)
	require.Greaterf(t, reassertIdx, runArgsIdx,
		"the PATH re-assertion must come AFTER run_args so Docker's last-wins rule makes it dominate: %v", runArgv)
}

// TestDockerAccount_PATHReassertSkippedWhenForwarded pins the operator
// exemption on the account path: PATH named in session_env_passthrough is
// the operator's own claim — filterAccountPassthrough keeps it (PATH is not
// an identity name) — so the bare `-e PATH` the forwarder emits is the only
// PATH entry and no image-PATH re-assertion is emitted to dominate it.
func TestDockerAccount_PATHReassertSkippedWhenForwarded(t *testing.T) {
	imageEnv := []string{"PATH=/img/bin:/usr/bin:/bin"}
	runCalled, _, runArgv := provisionDockerAccountImageEnv(t, "codex",
		[]string{"-e", "TZ=UTC"}, []string{"PATH"}, imageEnv)
	require.True(t, runCalled)
	require.Equal(t, -1, indexIn(runArgv, "PATH=/img/bin:/usr/bin:/bin"),
		"an operator-forwarded PATH must keep the operator's claim — no re-assertion: %v", runArgv)
	value, found := dockerEnvArg(runArgv, "PATH")
	require.True(t, found, "the operator's forwarded PATH must still reach the container: %v", runArgv)
	require.Equal(t, "PATH", value, "a forwarded name is emitted bare so the CLI env supplies the value")
}

// TestDockerAccount_PATHReassertSkippedWhenUninspectable pins the
// best-effort posture on the account path: when the image cannot be
// inspected (not pulled yet), no re-assertion is emitted — with run_args
// PATH refused, the container inherits the image's own PATH anyway, which is
// the same trusted source the re-assertion would have named.
func TestDockerAccount_PATHReassertSkippedWhenUninspectable(t *testing.T) {
	// The image inspect answer is an empty Config.Env, so dockerImageEnv
	// yields no PATH and the re-assertion must simply not appear — with
	// run_args PATH refused the container still inherits the image's own
	// PATH, the same trusted source the re-assertion would have named.
	runCalled, _, runArgv := provisionDockerAccountImageEnv(t, "codex",
		[]string{"-e", "TZ=UTC"}, nil, nil)
	require.True(t, runCalled)
	for _, arg := range runArgv {
		require.Falsef(t, strings.HasPrefix(arg, "PATH="),
			"an unreadable image PATH must not be re-asserted: %v", runArgv)
	}
}

// TestPrepareAccountUser_BusyBoxStatLocation is the review regression on
// #7529 R7's absolute stat: coreutils installs stat at /usr/bin/stat but
// BusyBox registers applets under BB_DIR_BIN (/bin/stat), so requiring the
// coreutils layout would break --account sessions on BusyBox-based BYO
// images. The owner probe must try the fixed absolute candidates in order —
// and never fall back to a PATH-resolved stat, which a repo-controlled PATH
// could point at its own binary.
func TestPrepareAccountUser_BusyBoxStatLocation(t *testing.T) {
	var statCalls []string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) == 0 || args[0] != "exec" {
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
		joined := strings.Join(args, " ")
		// Order matters: "/usr/bin/stat" contains "/bin/stat" as a substring.
		switch {
		case strings.Contains(joined, "/usr/bin/stat -c %u:%g"):
			statCalls = append(statCalls, "/usr/bin/stat")
			return nil, fmt.Errorf("no such file or directory")
		case strings.Contains(joined, "/bin/stat -c %u:%g"):
			statCalls = append(statCalls, "/bin/stat")
			return []byte("1000:1000\n"), nil
		case strings.Contains(joined, "stat -c %u:%g"):
			return nil, fmt.Errorf("a PATH-resolved stat must never be attempted: %v", args)
		case strings.Contains(joined, "/proc/self/mountinfo"):
			// prepareAccountUser reads the container's mount table before
			// chowning; answer with a plain root filesystem so the chown
			// of /af-home and /workspace proceeds.
			return mountTableAnswer(), nil
		default:
			return nil, nil
		}
	}))

	p := &dockerProvisioner{
		spec:        ProvisionSpec{Account: sessionenv.Account{Dir: t.TempDir()}},
		containerID: dockerCreatedID,
	}
	require.NoError(t, p.prepareAccountUser(), "the BusyBox stat location must satisfy the owner probe")
	require.Equal(t, "1000:1000", p.containerUser)
	require.Equal(t, []string{"/usr/bin/stat", "/bin/stat"}, statCalls,
		"the candidates are tried in order, coreutils first — the PATH is never walked")
}
