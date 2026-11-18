package session

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7531 tests: the late-loaded endpoint names the agent's own .env
// loader can restore after af's boundary (item 1), the common-allowlist
// completeness walk (item 2), and the migration warning reading the image
// the container actually runs (item 3).

// TestCredentialMountDeniedNames_CommonAllowlistIsClassified is item 2's
// walk: the completeness pin used to cover only the per-agent allowlist and
// conditional groups, so a common-allowlist name could sit reachable for
// every agent with no classification at all — the same silent-rot failure
// the per-agent pin exists to catch. Every common-allowlist name must land
// in exactly one of: the denied set or the shared kept set.
func TestCredentialMountDeniedNames_CommonAllowlistIsClassified(t *testing.T) {
	for agent := range agentCredentialFiles {
		denied := credentialMountDeniedNames(agent)
		for _, name := range sessionenv.CommonAllowlistNames() {
			_, isDenied := denied[name]
			_, isKept := credentialMountSharedKeptNames[name]
			require.Truef(t, isDenied || isKept,
				"agent %q: common-allowlist name %s is reachable for the agent but classified neither "+
					"as refused nor as kept — an unclassified credential or route passes through unopposed",
				agent, name)
			require.Falsef(t, isDenied && isKept,
				"agent %q: common-allowlist name %s is classified both as denied and as kept", agent, name)
		}
	}
}

// TestDockerCredentialRunArgs_RefusesLateLoadedEndpointNames is item 1's
// guard: gemini's .env loader can restore its endpoint names from a
// checked-in repository file, so a repo run_args entry naming the same
// variable is refused — af classifies them even though the agent's
// allowlist never admits them.
func TestDockerCredentialRunArgs_RefusesLateLoadedEndpointNames(t *testing.T) {
	for _, name := range []string{
		"CODE_ASSIST_ENDPOINT", "GOOGLE_GEMINI_BASE_URL",
		"GOOGLE_VERTEX_BASE_URL", "CODE_ASSIST_API_VERSION",
	} {
		err := validateCredentialDockerRunArgs([]string{"-e", name + "=https://repo.example"}, tmux.ProgramGemini)
		require.Errorf(t, err, "gemini: repo run_args setting late-loaded endpoint name %s must be refused", name)
		require.Contains(t, err.Error(), name, "the refusal must name the variable the operator has to remove")
		require.Contains(t, err.Error(), "session_env_passthrough", "the refusal must name the operator remedy")
	}
}

// TestCredentialMountEndpointPinsReachTheAgent is the reachability half of
// the endpoint fix (#7531 review): a `-e NAME=` pin or `-e NAME=value`
// re-assert on `docker run` is worthless if the agent-server's env filter
// strips the name before the agent execs — gemini's .env loader would see it
// unset and restore a repo value. Every endpoint name the boundary emits
// for gemini must therefore be allowlisted AND survive FilterForCommand at
// the exec, in exactly the emitted shape.
func TestCredentialMountEndpointPinsReachTheAgent(t *testing.T) {
	source := []string{
		"CODE_ASSIST_ENDPOINT=" + credentialMountFirstPartyNames[tmux.ProgramGemini]["CODE_ASSIST_ENDPOINT"],
		"GOOGLE_GEMINI_BASE_URL=",
		"GOOGLE_VERTEX_BASE_URL=",
		"CODE_ASSIST_API_VERSION=",
	}
	allowlist := sortedNamesToSet(sessionenv.AgentAllowlistNames(tmux.ProgramGemini))
	for _, entry := range source {
		name, _, _ := strings.Cut(entry, "=")
		require.Containsf(t, allowlist, name,
			"gemini: %s must be allowlisted so the pin or re-assert reaches the agent's exec environment", name)
	}
	got := sortedNamesToSet(sessionenv.FilterForCommand(source, "gemini", "gemini", nil))
	for _, entry := range source {
		require.Containsf(t, got, entry,
			"gemini: the exec filter must carry %q through to the agent — a stripped name is unset to the .env loader", entry)
	}
}

// TestCredentialMountDeniedReason_EndpointClasses covers the refusal's
// because-clause for the new classes so the operator-facing message names
// the mechanism each defends.
func TestCredentialMountDeniedReason_EndpointClasses(t *testing.T) {
	for _, name := range []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_VERTEX_BASE_URL", "CODE_ASSIST_API_VERSION"} {
		reason, ok := credentialMountDeniedReason(tmux.ProgramGemini, name)
		require.True(t, ok, "gemini: %s must have a refusal reason", name)
		require.Contains(t, reason, "where the agent sends", "gemini: %s is a routing name", name)
	}
	for name := range credentialMountFirstPartyNames[tmux.ProgramGemini] {
		reason, ok := credentialMountDeniedReason(tmux.ProgramGemini, name)
		require.True(t, ok, "gemini: %s must have a refusal reason", name)
		require.Contains(t, reason, "first-party", "gemini: %s's reason must name the re-assert mechanism", name)
	}
}

// TestRunContainer_ReassertsFirstPartyEndpointAndPinsLateNames is the
// end-to-end argv pin for item 1: on a gemini credential-mount session the
// `docker run` argv carries the first-party endpoint re-assert AFTER
// run_args (so a repo entry — even one that escaped the lexical guard —
// cannot dominate it), and the falsy-safe late routing names pinned
// defined-and-empty so the .env loader sees them as claimed.
func TestRunContainer_ReassertsFirstPartyEndpointAndPinsLateNames(t *testing.T) {
	runCalled, _, runArgv := provisionDockerCredentialGrant(t, tmux.ProgramGemini,
		[]string{"--add-host", "repo-host:127.0.0.1"})
	require.True(t, runCalled, "the provisioner must reach docker run")

	reassertAt := -1
	pinFor := map[string]int{}
	addHostAt := -1
	for i, arg := range runArgv {
		if arg == "--add-host" {
			addHostAt = i
		}
		if arg == "CODE_ASSIST_ENDPOINT=https://cloudcode-pa.googleapis.com" {
			reassertAt = i
		}
		for _, name := range []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_VERTEX_BASE_URL", "CODE_ASSIST_API_VERSION"} {
			if arg == name+"=" {
				pinFor[name] = i
			}
		}
	}
	require.NotEqual(t, -1, addHostAt, "the run_args entry must appear in argv")
	require.Greater(t, reassertAt, addHostAt,
		"the first-party endpoint re-assert must come after run_args so Docker's last-wins rule dominates a repo value")
	for _, name := range []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_VERTEX_BASE_URL", "CODE_ASSIST_API_VERSION"} {
		require.Greater(t, pinFor[name], addHostAt,
			"the %s pin must come after run_args so a repo value cannot dominate it", name)
	}
}

// TestRunContainer_ReassertsGitConfigRoots is the argv pin for the
// path-valued names (#7531 review): GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM are
// re-asserted to working paths under the boundary's own roots — never
// defined-empty, which would break `git config --global` — and
// GIT_CONFIG_NOSYSTEM, a boolean, stays pinned defined-and-empty.
func TestRunContainer_ReassertsGitConfigRoots(t *testing.T) {
	runCalled, _, runArgv := provisionDockerCredentialGrant(t, tmux.ProgramGemini, nil)
	require.True(t, runCalled)

	joined := strings.Join(runArgv, "\x00")
	require.Contains(t, joined, "GIT_CONFIG_GLOBAL=",
		"the global config root must be re-asserted to a working path")
	require.NotContains(t, joined, "-e\x00GIT_CONFIG_GLOBAL=\x00",
		"an empty GIT_CONFIG_GLOBAL breaks `git config --global` — the re-assert must carry a real path")
	require.NotContains(t, joined, "GIT_CONFIG_GLOBAL=/dev/null",
		"/dev/null fails git config writes with EACCES — the re-assert must be a writable location")
	require.Contains(t, joined, "GIT_CONFIG_SYSTEM=/etc/gitconfig",
		"the system config root must be re-asserted to git's own default path")
	require.Contains(t, joined, "-e\x00GIT_CONFIG_NOSYSTEM=\x00",
		"a boolean name pins defined-and-empty — the default behavior")
	// The re-asserted path lives under the re-asserted HOME.
	for i, arg := range runArgv {
		if strings.HasPrefix(arg, "GIT_CONFIG_GLOBAL=") {
			require.Equal(t, runArgv[i-1], "-e")
			require.True(t, strings.HasSuffix(arg, "/.gitconfig"),
				"the global config root must point at a writable file under the session home: %s", arg)
		}
	}
}

// TestRunContainer_OperatorForwardKeepsFirstPartyEndpoint is the
// operator-exemption half: a name in session_env_passthrough carries the
// operator's own value, so the first-party re-assert must NOT be emitted for
// it (the forwarded -e NAME resolves through the operator's environment).
func TestRunContainer_OperatorForwardKeepsFirstPartyEndpoint(t *testing.T) {
	t.Setenv("CODE_ASSIST_ENDPOINT", "https://codeassist.corp.example")
	runCalled, _, runArgv := provisionDockerCredentialGrantPassthrough(t, tmux.ProgramGemini,
		nil, []string{"CODE_ASSIST_ENDPOINT"})
	require.True(t, runCalled)
	joined := strings.Join(runArgv, "\x00")
	require.Contains(t, joined, "-e\x00CODE_ASSIST_ENDPOINT\x00",
		"the operator's forwarded name must ride argv as a bare -e so it resolves through the operator's env")
	require.NotContains(t, joined, "CODE_ASSIST_ENDPOINT=https://cloudcode-pa.googleapis.com",
		"the first-party re-assert must not replace the operator's forwarded value")
	require.NotContains(t, joined, "codeassist.corp.example",
		"the operator's value must never enter argv")
}

// TestWarnBlankedCredentialNames_ReadsTheImageTheContainerRuns is item 3's
// property: the migration warning names what the image the container
// ACTUALLY runs had set — resolved off the created container — not whatever
// the configured tag inspected as before docker run pulled or updated it.
// Here the tag's inspect advertises one pinned name while the resolved
// image's advertises another; the warning must name the resolved image's
// and never the tag's.
func TestWarnBlankedCredentialNames_ReadsTheImageTheContainerRuns(t *testing.T) {
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "inspect "+dockerCreatedID):
			return []byte("sha256:resolved-after-pull\n"), nil
		case strings.HasPrefix(joined, "image inspect sha256:resolved-after-pull"):
			return []byte(`["HTTPS_PROXY=http://corp-proxy:8080","TZ=UTC"]`), nil
		case strings.HasPrefix(joined, "image inspect "):
			return []byte(`["SSL_CERT_FILE=/tag-only.pem"]`), nil
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	}))

	p := &dockerProvisioner{
		program:           tmux.ProgramGemini,
		image:             "example.invalid/agent:latest",
		containerID:       dockerCreatedID,
		credentialMounts:  []string{"mount"},
		credentialEnvPins: credentialMountPinnedNames(tmux.ProgramGemini, nil),
		spec:              ProvisionSpec{Program: "gemini", Title: "t"},
	}
	buf := captureWarningLog(t)
	p.warnBlankedCredentialNames()
	out := buf.String()
	require.Contains(t, out, "HTTPS_PROXY",
		"the warning must name what the image the container runs had set")
	require.NotContains(t, out, "SSL_CERT_FILE",
		"the warning must not describe the configured tag's pre-run inspect — that is a different image")
}

// TestWarnBlankedCredentialNames_SaysWhenImageEnvUnreadable is the other
// half of item 3: when the container's image environment cannot be
// inspected, the log must SAY it could not — the warning must not silently
// describe a subset — and still report the names run_args carried.
func TestWarnBlankedCredentialNames_SaysWhenImageEnvUnreadable(t *testing.T) {
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("no such image")
	}))

	p := &dockerProvisioner{
		program:           tmux.ProgramGemini,
		image:             "example.invalid/agent:latest",
		containerID:       dockerCreatedID,
		credentialMounts:  []string{"mount"},
		credentialEnvPins: credentialMountPinnedNames(tmux.ProgramGemini, nil),
		runArgs:           []string{"-e", "NODE_EXTRA_CA_CERTS=/repo/ca.pem"},
		spec:              ProvisionSpec{Program: "gemini", Title: "t"},
	}
	buf := captureWarningLog(t)
	p.warnBlankedCredentialNames()
	out := buf.String()
	require.Contains(t, out, "could not inspect the environment of the image the container runs",
		"an unreadable image environment must be logged, not silently skipped")
	require.Contains(t, out, "NODE_EXTRA_CA_CERTS",
		"the warning still names what run_args set even when the image side cannot be read")
}

// TestRunContainer_ReassertsProgramSelectors is the second half of the
// "re-asserted name keeps the program WORKING" property (#7531 review): the
// shared program-selector names are emitted with real working values after
// run_args, never as empty pins — measured on git 2.43, a defined-empty
// GIT_SSH_COMMAND names "" as the executable and every SSH transport fails
// `cannot run : No such file or directory`, which would break af's own
// cloneWorkspace for ssh origins.
func TestRunContainer_ReassertsProgramSelectors(t *testing.T) {
	runCalled, _, runArgv := provisionDockerCredentialGrant(t, tmux.ProgramGemini,
		[]string{"--add-host", "repo.internal:10.0.0.1"})
	require.True(t, runCalled)

	// The contract is literal, not table-derived: these are the working
	// values the boundary must emit — `ssh` restores git's own default via
	// the re-asserted image PATH, /bin/false is a working program that never
	// answers a prompt, /bin/cat keeps tty paging readable.
	want := map[string]string{
		"GIT_SSH":         "ssh",
		"GIT_SSH_COMMAND": "ssh",
		"GIT_ASKPASS":     "/bin/false",
		"SSH_ASKPASS":     "/bin/false",
		"EDITOR":          "/bin/false",
		"VISUAL":          "/bin/false",
		"PAGER":           "/bin/cat",
	}
	for name, value := range want {
		entry := name + "=" + value
		idx := indexIn(runArgv, entry)
		require.Greaterf(t, idx, 0,
			"%s must be re-asserted to a working value (%q), not pinned empty — git runs a defined-empty selector literally", name, entry)
		require.Equal(t, "-e", runArgv[idx-1], "%s must be a -e emission", entry)
		require.Greater(t, idx, indexIn(runArgv, "--add-host"),
			"%s must land after run_args so last-wins dominates a repo-supplied value", entry)
	}
	joined := strings.Join(runArgv, "\x00")
	for name := range want {
		require.NotContains(t, joined, "-e\x00"+name+"=\x00",
			"%s pinned defined-and-empty is the broken form this test exists to forbid", name)
	}
}

// TestRunContainer_OperatorForwardKeepsProgramSelector: an operator who
// forwards a program-selector name through session_env_passthrough keeps
// their own value — af must not re-assert over it.
func TestRunContainer_OperatorForwardKeepsProgramSelector(t *testing.T) {
	runCalled, _, runArgv := provisionDockerCredentialGrantPassthrough(t, tmux.ProgramGemini,
		nil, []string{"GIT_SSH_COMMAND"})
	require.True(t, runCalled)
	require.Equal(t, -1, indexIn(runArgv, "GIT_SSH_COMMAND="+credentialMountSharedProgramEntries["GIT_SSH_COMMAND"]),
		"an operator-forwarded selector keeps the operator's value — the re-assert must not override it")
}

// TestProvision_WarnsBeforeDependentStepFails is the review finding on the
// warning's position: a blanked name a dependent step needed (a proxy or CA
// for the clone) makes THAT step fail, and the error path reaps the
// container — so the warning must fire immediately after the container
// exists, before any dependent step can take the session down. Here the
// step after container creation fails and the warning still names the
// image-supplied name.
func TestProvision_WarnsBeforeDependentStepFails(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCredFile(t, filepath.Join(home, ".gemini/oauth_creds.json"))
	_ = forceCredentialMountMode(t)
	cfg := config.DefaultConfig()
	cfg.DockerMountAgentCredentials = true
	require.NoError(t, config.SaveConfig(cfg))

	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "docker",
		"docker":  map[string]any{"image": "example.invalid/agent:latest"},
	})

	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	t.Cleanup(SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af")))
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "info":
			return []byte("credential-test-engine\n"), nil
		case len(args) > 0 && args[0] == "run":
			return []byte(dockerCreatedID + "\n"), nil
		case len(args) > 0 && args[0] == "inspect":
			return []byte("sha256:resolved\n"), nil
		case len(args) > 1 && args[0] == "image" && args[1] == "inspect":
			return []byte(`["HTTPS_PROXY=http://corp-proxy:8080"]`), nil
		case len(args) > 0 && (args[0] == "rm" || args[0] == "stop" || args[0] == "kill"):
			return nil, nil // the failure-path reap may clean up freely
		default:
			// Every dependent step after container creation fails here —
			// the warning must already have fired by then.
			return nil, fmt.Errorf("dependent step failed: %v", args)
		}
	}))

	buf := captureWarningLog(t)
	_, err := (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "warn-order",
		Program:  tmux.ProgramGemini,
		CloneURL: "file:///fixture.git",
	})
	require.Error(t, err, "this fixture's dependent steps must fail — the test proves the warning precedes them")
	out := buf.String()
	require.Contains(t, out, "HTTPS_PROXY",
		"the blanked-name warning must fire before the dependent step that the blanked name broke")
	require.Contains(t, out, "session_env_passthrough",
		"the warning must still carry its operator remedy")
}

// TestCredentialMount_SessionMarkersSurviveTheExecBoundary is the Codex P2
// regression property (#7531): af's OWN session markers — the names
// sessionEnvFlags stamps into every pane environment — are trusted values
// from af itself, not a repo source. Classifying them pinnable put them in
// --session-env-empty, so the exec shim re-asserted them defined-and-empty
// AFTER tmux installed af's values, and the agent's children lost the
// markers markedOrphanProcesses needs to attribute and reap them inside
// the container. Every marker must stay OUT of the pins and the
// --session-env-empty argv while still being refused to repo run_args.
func TestCredentialMount_SessionMarkersSurviveTheExecBoundary(t *testing.T) {
	markers := []string{
		tmux.EnvMarkerSession, tmux.EnvMarkerGeneration, tmux.EnvMarkerHome, tmux.EnvMarkerInstance,
	}
	for _, name := range markers {
		for _, agent := range []string{tmux.ProgramGemini, tmux.ProgramClaude} {
			require.NotContains(t, credentialMountPinnedNames(agent, nil), name,
				"%s is af's own marker — pinning it empty erases the membership proof at the exec shim", name)
		}
	}

	// The argv level: --session-env-empty is what the exec shim re-asserts;
	// a marker there is the blank-after-tmux order the finding describes.
	p := &dockerProvisioner{
		program:           tmux.ProgramGemini,
		credentialMounts:  []string{"mount"},
		credentialEnvPins: credentialMountPinnedNames(tmux.ProgramGemini, nil),
		spec:              ProvisionSpec{Program: "gemini", Title: "markers"},
	}
	argv, err := p.agentServerCommand()
	require.NoError(t, err)
	for i, name := range markers {
		require.NotContains(t, argv, "--session-env-empty "+name,
			"--session-env-empty %s would blank the marker after tmux installed af's value (marker %d)", name, i)
	}

	// The deny half stays: a repository run_args marker is still refused —
	// only af may write these names.
	for _, name := range markers {
		_, denied := credentialMountDeniedNames(tmux.ProgramGemini)[name]
		require.True(t, denied || name == tmux.EnvMarkerInstance,
			"%s must still be refused to repository run_args (AF_INSTANCE_ID is unallowlisted — unreachable either way)", name)
	}
}
