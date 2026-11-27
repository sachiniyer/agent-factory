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
		err := validateCredentialDockerRunArgs([]string{"-e", name + "=https://repo.example"}, tmux.ProgramGemini, nil)
		require.Errorf(t, err, "gemini: repo run_args setting late-loaded endpoint name %s must be refused", name)
		require.Contains(t, err.Error(), name, "the refusal must name the variable the operator has to remove")
		require.Contains(t, err.Error(), "session_env_passthrough", "the refusal must name the operator remedy")
	}
	// GOOGLE_CLOUD_UNIVERSE_DOMAIN is read only by bundled library code, but
	// it retargets the credential-bearing telemetry dial host, so repo
	// run_args setting it is refused on the same terms as the endpoint names.
	err := validateCredentialDockerRunArgs([]string{"-e", "GOOGLE_CLOUD_UNIVERSE_DOMAIN=evil.example.com"}, tmux.ProgramGemini, nil)
	require.Errorf(t, err, "gemini: repo run_args setting GOOGLE_CLOUD_UNIVERSE_DOMAIN must be refused — it retargets the telemetry dial host")
	require.Contains(t, err.Error(), "GOOGLE_CLOUD_UNIVERSE_DOMAIN")
	require.Contains(t, err.Error(), "session_env_passthrough")
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
		"GOOGLE_CLOUD_UNIVERSE_DOMAIN=" + credentialMountFirstPartyNames[tmux.ProgramGemini]["GOOGLE_CLOUD_UNIVERSE_DOMAIN"],
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
	// The universe domain is re-asserted to a VALUE, never pinned empty: the
	// bundled-transport ?? read would treat "" as the dial host `logging.`,
	// so an empty pin breaks telemetry while a repo `.env` still cannot claim
	// the name (it is refused to run_args and dominated by the re-assert).
	pins := credentialMountPinnedNames(tmux.ProgramGemini, nil)
	require.NotContains(t, pins, "GOOGLE_CLOUD_UNIVERSE_DOMAIN",
		"the universe domain takes a first-party re-assert, not an empty pin")
	_, denied := credentialMountDeniedNames(tmux.ProgramGemini)["GOOGLE_CLOUD_UNIVERSE_DOMAIN"]
	require.True(t, denied, "GOOGLE_CLOUD_UNIVERSE_DOMAIN must be refused to repository run_args")
	reasserts := credentialMountReassertEntries(tmux.ProgramGemini, nil, nil)
	require.Contains(t, reasserts, "GOOGLE_CLOUD_UNIVERSE_DOMAIN=googleapis.com",
		"the first-party re-assert must carry the compiled default onto docker run")
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

// TestGeminiBundleArtifact_MountClassesLand is the mount-path half of the
// #7662 landing check — the account-path half lives beside the artifact in
// internal/sessionenv. Every artifact entry's mount class must reach the
// table that delivers it: pin names are refused to run_args AND emitted
// defined-and-empty (denial without the pin would leave the .env loader's
// claim), reassert names carry the measured first-party value, shared
// names are claimed by the agent-agnostic boundary tables rather than the
// artifact-derived sets, and denied-no-pin names are exactly the
// pane-boundary set a pin must never erase.
func TestGeminiBundleArtifact_MountClassesLand(t *testing.T) {
	denied := credentialMountDeniedNames(tmux.ProgramGemini)
	pinned := sortedNamesToSet(credentialMountPinnedNames(tmux.ProgramGemini, nil))
	for _, r := range sessionenv.GeminiBundleEnvReads {
		switch r.Mount {
		case sessionenv.GeminiBundleEnvPin:
			require.Containsf(t, denied, r.Name,
				"%s is pin-classified but not refused to repo run_args", r.Name)
			require.Containsf(t, pinned, r.Name,
				"%s is pin-classified but reaches no defined-empty pin — the .env loader could claim it", r.Name)
		case sessionenv.GeminiBundleEnvReassert:
			require.Equalf(t, r.Value, credentialMountFirstPartyNames[tmux.ProgramGemini][r.Name],
				"%s: artifact and mount table disagree on the re-asserted value", r.Name)
		case sessionenv.GeminiBundleEnvShared:
			require.Containsf(t, denied, r.Name,
				"%s is shared-boundary but no boundary table claims it", r.Name)
			require.NotContainsf(t, pinned, r.Name,
				"%s is shared-boundary — it re-asserts a real value, an empty pin would erase it", r.Name)
		case sessionenv.GeminiBundleEnvNoPin:
			require.Containsf(t, credentialMountPaneBoundaryNames, r.Name,
				"%s is denied-no-pin — it must be the pane-boundary set a pin would erase", r.Name)
		}
	}
}

// TestRunContainer_ReassertsFirstPartyEndpointAndPinsLateNames is the
// end-to-end argv pin for item 1: on a gemini credential-mount session the
// `docker run` argv carries the first-party re-asserts AFTER run_args (so a
// repo entry — even one that escaped the lexical guard — cannot dominate
// them), and the falsy-safe late routing names pinned defined-and-empty so
// the .env loader sees them as claimed.
func TestRunContainer_ReassertsFirstPartyEndpointAndPinsLateNames(t *testing.T) {
	runCalled, _, runArgv := provisionDockerCredentialGrant(t, tmux.ProgramGemini,
		[]string{"--add-host", "repo-host:127.0.0.1"})
	require.True(t, runCalled, "the provisioner must reach docker run")

	addHostAt := -1
	reassertFor := map[string]int{}
	pinFor := map[string]int{}
	reasserts := []string{
		"CODE_ASSIST_ENDPOINT=https://cloudcode-pa.googleapis.com",
		"GOOGLE_CLOUD_UNIVERSE_DOMAIN=googleapis.com",
	}
	pins := []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_VERTEX_BASE_URL", "CODE_ASSIST_API_VERSION"}
	for i, arg := range runArgv {
		if arg == "--add-host" {
			addHostAt = i
		}
		for _, entry := range reasserts {
			if arg == entry {
				reassertFor[entry] = i
			}
		}
		for _, name := range pins {
			if arg == name+"=" {
				pinFor[name] = i
			}
		}
	}
	require.NotEqual(t, -1, addHostAt, "the run_args entry must appear in argv")
	for _, entry := range reasserts {
		require.Greater(t, reassertFor[entry], addHostAt,
			"the %s re-assert must come after run_args so Docker's last-wins rule dominates a repo value", entry)
	}
	for _, name := range pins {
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
// regression property (#7531): the names the pane boundary itself installs —
// af's OWN session markers from sessionEnvFlags, plus TMUX and TMUX_PANE
// which the tmux server stamps for every pane — are trusted values, not a
// repo source. Classifying them pinnable put them in --session-env-empty,
// so the exec shim re-asserted them defined-and-empty AFTER tmux installed
// the real values: the agent's children lost the markers
// markedOrphanProcesses needs to attribute and reap them, and blanked TMUX
// names broke Claude Code's tmux detection and af's own in-pane `--self`
// (currentTmuxName). Every pane-boundary name must stay OUT of the pins
// and the --session-env-empty argv while still being refused to repo
// run_args.
func TestCredentialMount_SessionMarkersSurviveTheExecBoundary(t *testing.T) {
	markers := []string{
		tmux.EnvMarkerSession, tmux.EnvMarkerGeneration, tmux.EnvMarkerHome, tmux.EnvMarkerInstance,
		"AF_TESTGUARD_RUN", "TMUX", "TMUX_PANE",
	}
	for _, name := range markers {
		for _, agent := range []string{tmux.ProgramGemini, tmux.ProgramClaude} {
			require.NotContains(t, credentialMountPinnedNames(agent, nil), name,
				"%s is installed at the pane boundary — pinning it empty erases it at the exec shim", name)
		}
	}

	// The argv level: --session-env-empty is what the exec shim re-asserts;
	// a pane-boundary name there is the blank-after-tmux order the finding
	// describes.
	p := &dockerProvisioner{
		program:           tmux.ProgramGemini,
		credentialMounts:  []string{"mount"},
		credentialEnvPins: credentialMountPinnedNames(tmux.ProgramGemini, nil),
		spec:              ProvisionSpec{Program: "gemini", Title: "markers"},
	}
	argv, err := p.agentServerCommand()
	require.NoError(t, err)
	fields := strings.Fields(argv)
	emptied := make(map[string]struct{})
	for i, f := range fields {
		if f == "--session-env-empty" && i+1 < len(fields) {
			emptied[fields[i+1]] = struct{}{}
		}
	}
	for i, name := range markers {
		_, blank := emptied[name]
		require.False(t, blank,
			"--session-env-empty %s would blank the value after tmux installed it (marker %d)", name, i)
	}

	// The deny half stays: a repository run_args marker is still refused —
	// only af may write these names.
	for _, name := range markers {
		_, denied := credentialMountDeniedNames(tmux.ProgramGemini)[name]
		require.True(t, denied, "%s must still be refused to repository run_args", name)
	}
}

// TestCredentialMount_SelectorGroupForwardingExemptsTheWholeGroup is the
// R2(a) property (#7531): the selector names are precedence groups — a
// tool reads GIT_SSH_COMMAND before GIT_SSH, GIT_ASKPASS before
// SSH_ASKPASS, VISUAL before EDITOR — so forwarding ANY member exempts the
// WHOLE group from the re-assert, and the unforwarded member is reseated
// to the operator's forwarded value so an image-supplied sibling cannot
// keep precedence over the operator's program.
func TestCredentialMount_SelectorGroupForwardingExemptsTheWholeGroup(t *testing.T) {
	// Without a claim every member is still re-asserted (the bound).
	for _, group := range credentialMountSelectorGroups {
		for _, name := range group {
			require.Contains(t, credentialMountReassertEntries(tmux.ProgramClaude, nil, nil), name+"="+credentialMountSharedProgramEntries[name],
				"unclaimed %s keeps its working re-assert", name)
		}
	}
	// Forwarding either member SET in the daemon env exempts BOTH, in
	// either order, and reseats the unforwarded sibling to the operator's
	// value in the sibling's grammar.
	for _, group := range credentialMountSelectorGroups {
		for _, forwarded := range group {
			var sibling string
			if forwarded == group[0] {
				sibling = group[1]
			} else {
				sibling = group[0]
			}
			entries := credentialMountReassertEntries(tmux.ProgramClaude,
				[]string{forwarded}, []string{forwarded + "=/op-program"})
			for _, entry := range entries {
				name, _, _ := strings.Cut(entry, "=")
				require.NotEqual(t, group[0], name, "forwarding %s must exempt %s from the re-assert", forwarded, name)
				require.NotEqual(t, group[1], name, "forwarding %s must exempt %s from the re-assert", forwarded, name)
			}
			names, synth := credentialMountSelectorForwarding(
				[]string{forwarded}, []string{forwarded + "=/op-program"})
			require.Equal(t, []string{sibling}, names,
				"the unforwarded sibling must be re-emitted so the image's value cannot stand")
			require.Equal(t, []string{sibling + "=" + credentialMountSelectorReseat(sibling, "/op-program")}, synth,
				"the sibling resolves to the operator's forwarded value in its own grammar")
		}
	}
	// Ungrouped selectors are untouched: forwarding EDITOR does not exempt
	// PAGER, and PAGER is still re-asserted.
	require.Contains(t, credentialMountReassertEntries(tmux.ProgramClaude, []string{"EDITOR"}, []string{"EDITOR=vim"}), "PAGER=/bin/cat")
}

// TestCredentialMount_SelectorReseat_TranslatesGrammar is the R2a-P1
// property (#7531): the reseated sibling names the same program in the
// SIBLING's grammar. GIT_SSH is a path exec'd directly; GIT_SSH_COMMAND
// is a shell command line — a path with whitespace or metacharacters
// copied verbatim splits into words and the operator's program never
// runs (measured git 2.43: '/op dir/op-ssh' → `/op: not found`). The
// sibling value is shell-quoted; the reverse direction keeps af's
// working default when the command is not a bare path.
func TestCredentialMount_SelectorReseat_TranslatesGrammar(t *testing.T) {
	// Path with a space AND a quote → single-quoted command line.
	require.Equal(t, `'/op dir/o'"'"'p-ssh'`,
		credentialMountSelectorReseat("GIT_SSH_COMMAND", `/op dir/o'p-ssh`),
		"a path with whitespace and a quote must land in GIT_SSH_COMMAND as one shell word")
	// End-to-end through the twin forwarding: the synthesized env entry
	// carries the quoted command, never the raw path.
	_, synth := credentialMountSelectorForwarding([]string{"GIT_SSH"}, []string{`GIT_SSH=/op dir/o'p-ssh`})
	require.Equal(t, []string{`GIT_SSH_COMMAND='/op dir/o'"'"'p-ssh'`}, synth)
	// A plain path stays bare — shellquote leaves safe words unquoted.
	require.Equal(t, "/op-ssh", credentialMountSelectorReseat("GIT_SSH_COMMAND", "/op-ssh"))
	// Command → path direction: a bare path translates verbatim; a real
	// command line cannot be a path, so the sibling keeps af's `ssh`
	// (git's own default — never a repository program, and the forwarded
	// command wins precedence anyway).
	require.Equal(t, "/op-ssh", credentialMountSelectorReseat("GIT_SSH", "/op-ssh"))
	require.Equal(t, "ssh", credentialMountSelectorReseat("GIT_SSH", "/op-ssh -p 2222"),
		"a command line cannot be reseated as a path — af's own default is the floor")
	_, synth = credentialMountSelectorForwarding([]string{"GIT_SSH_COMMAND"}, []string{"GIT_SSH_COMMAND=/op-ssh -p 2222"})
	require.Equal(t, []string{"GIT_SSH=ssh"}, synth)
	// Same-grammar groups reseat verbatim.
	require.Equal(t, "/op dir/ask", credentialMountSelectorReseat("GIT_ASKPASS", "/op dir/ask"))
	require.Equal(t, "vim --cmd x", credentialMountSelectorReseat("VISUAL", "vim --cmd x"))
}

// TestCredentialMount_SelectorGroup_UnclaimedKeepsReassert is the R2a-P2
// property (#7531): the exemption and the reseat share ONE claim test —
// forwarded AND set in the daemon environment. A member forwarded but
// unset (common under systemd) does not claim the group, so the
// unforwarded member keeps af's working re-assert and an image-supplied
// sibling cannot slip in under precedence.
func TestCredentialMount_SelectorGroup_UnclaimedKeepsReassert(t *testing.T) {
	// SSH_ASKPASS forwarded but NOT in the daemon env: the group is
	// unclaimed, so GIT_ASKPASS keeps /bin/false — the image's
	// /workspace/.x/ask can never run.
	entries := credentialMountReassertEntries(tmux.ProgramClaude, []string{"SSH_ASKPASS"}, nil)
	require.Contains(t, entries, "GIT_ASKPASS=/bin/false",
		"a forwarded-but-unset member must not exempt the group — the sibling keeps its re-assert")
	for _, entry := range entries {
		name, _, _ := strings.Cut(entry, "=")
		require.NotEqual(t, "SSH_ASKPASS", name,
			"the forwarded name itself still keeps the operator's claim")
	}
	// Same for the other two groups' higher-precedence members.
	entries = credentialMountReassertEntries(tmux.ProgramClaude, []string{"GIT_SSH"}, nil)
	require.Contains(t, entries, "GIT_SSH_COMMAND=ssh")
	entries = credentialMountReassertEntries(tmux.ProgramClaude, []string{"EDITOR"}, nil)
	require.Contains(t, entries, "VISUAL=/bin/false")
	// And the reseat agrees: an unset forwarded member emits nothing.
	names, synth := credentialMountSelectorForwarding([]string{"SSH_ASKPASS"}, nil)
	require.Empty(t, names)
	require.Empty(t, synth)
}

// TestDockerMountAgentCredentials_SelectorTwinRidesRunArgvAndClientEnv is the
// group claim end-to-end: the sibling's bare `-e NAME` lands in the run argv
// (name only) and the client environment synthesizes the operator's value.
func TestDockerMountAgentCredentials_SelectorTwinRidesRunArgvAndClientEnv(t *testing.T) {
	t.Setenv("GIT_SSH", "/op-ssh")
	p := &dockerProvisioner{
		spec: ProvisionSpec{
			Title:                 "selector-twin-test",
			Program:               tmux.ProgramClaude,
			SessionEnvPassthrough: []string{"GIT_SSH"},
		},
		program:          tmux.ProgramClaude,
		credentialMounts: []string{"-v", "/h/cred:/c/cred:ro"},
	}
	require.Contains(t, p.containerEnvironmentNames(), "GIT_SSH_COMMAND",
		"the unforwarded sibling must be emitted so the image's value cannot stand")
	require.Contains(t, p.dockerEnvironment(), "GIT_SSH_COMMAND=/op-ssh",
		"the client env must resolve the sibling to the operator's forwarded program")
	p.spec.SessionEnvPassthrough = nil
	require.NotContains(t, p.containerEnvironmentNames(), "GIT_SSH_COMMAND")
	require.NotContains(t, p.dockerEnvironment(), "GIT_SSH_COMMAND=/op-ssh")
}

// TestCredentialMountOverriddenWarning_NamesReassertedToo is the R2(b)
// property (#7531): the migration warning names every image- or
// run_args-set name af overrode — blanked OR re-asserted — so nothing is
// overridden silently. A re-asserted EDITOR=vim in the image is named, and
// the message says the override may be either a pin or a re-assert.
func TestCredentialMountOverriddenWarning_NamesReassertedToo(t *testing.T) {
	p := &dockerProvisioner{
		program:           tmux.ProgramClaude,
		credentialMounts:  []string{"mount"},
		credentialEnvPins: credentialMountPinnedNames(tmux.ProgramClaude, nil),
		spec:              ProvisionSpec{Program: "claude", Title: "warn"},
		containerID:       "cid",
	}
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "inspect":
			return []byte("sha256:resolved\n"), nil
		case len(args) > 1 && args[0] == "image" && args[1] == "inspect":
			return []byte(`["EDITOR=vim","HTTPS_PROXY=http://corp:8080","TZ=UTC"]`), nil
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	}))
	buf := captureWarningLog(t)
	p.warnBlankedCredentialNames()
	out := buf.String()
	require.Contains(t, out, "EDITOR",
		"an image-set selector af re-asserts must be named — /bin/false silently replacing vim is the R2 finding")
	require.Contains(t, out, "HTTPS_PROXY", "a blanked pin is still named")
	require.NotContains(t, out, "TZ", "an unoverridden image name is not named")
	require.Contains(t, out, "pinned empty or re-asserted")
}

// TestRunContainer_BlanksPaneBoundaryNamesForExecChildren is #7544's
// marker-authority property: af's session markers and tmux's pane names
// are blanked at `docker run` — never at the exec shim — so the container
// environment every `docker exec` child inherits (the agent-server, the
// provisioning steps) carries defined-empty names instead of a forged
// image ENV value. The in-pane agent still gets the real values: tmux
// stamps them per pane over this baseline, and the blank TMUX is safe
// because tmux itself treats an empty value as unset (measured on tmux
// 3.4 and 3.2a) — the baseline cannot trip the nested-session guard.
func TestRunContainer_BlanksPaneBoundaryNamesForExecChildren(t *testing.T) {
	imageEnv := []string{
		"AF_SESSION_GEN=image-spoof-gen", "AF_SESSION=image-spoof-session",
		"TMUX=/tmp/tmux-0/fake,1,0", "TZ=UTC",
	}
	runCalled, _, runArgv := provisionDockerCredentialGrantImageEnv(t, tmux.ProgramGemini,
		[]string{"--add-host", "repo.internal:10.0.0.1"}, nil, imageEnv)
	require.True(t, runCalled)

	for _, name := range []string{
		"AF_SESSION", "AF_SESSION_GEN", "AF_HOME", "AF_INSTANCE_ID", "AF_TESTGUARD_RUN",
		"TMUX", "TMUX_PANE",
	} {
		entry := name + "="
		idx := indexIn(runArgv, entry)
		require.Greaterf(t, idx, 0,
			"%s must be blanked at docker run — the container env is what docker exec children inherit", name)
		require.Equal(t, "-e", runArgv[idx-1], "%s must be a -e emission", entry)
		require.Greater(t, idx, indexIn(runArgv, "--add-host"),
			"%s must land after run_args so last-wins dominates an image ENV or repo entry", entry)
	}

	// Blanked, never re-set with a value: no pane-boundary name may carry a
	// value in the run argv — the real values are tmux's per-pane stamps,
	// and the forged image values must not survive anywhere in the docker
	// invocation af built.
	joined := strings.Join(runArgv, "\x00")
	for _, forged := range []string{"image-spoof-gen", "image-spoof-session", "tmux-0/fake"} {
		require.NotContains(t, joined, forged,
			"the image's forged marker value must not reach the container env at all")
	}
}

// TestCredentialMount_LoaderFilledNamesArePinnedEmpty is #7544's dotenv
// property: the classes of name an agent's own .env loader fills when
// unset are pinned defined-and-empty on a credential-mount session, so
// the loader's only-if-unset rule leaves af's claim alone — and a class
// is pinned only when EVERY name the agent's read consults is pinned, so
// the test walks each 0.42.0 read expression link by link. Measured
// falsy-safe on @google/gemini-cli 0.42.0: the auth source is
// CLOUD_SHELL === "true" || GEMINI_CLI_USE_COMPUTE_ADC === "true", the
// metadata endpoint is GCE_METADATA_IP || GCE_METADATA_HOST ||
// HOST_ADDRESS (a compiled constant — not an env read, so nothing to
// pin), and git treats GIT_CONFIG_COUNT="" as count 0 — making
// every indexed GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* pair inert — and an
// empty GIT_CONFIG_PARAMETERS as zero -c pairs.
func TestCredentialMount_LoaderFilledNamesArePinnedEmpty(t *testing.T) {
	// The git-env class is shared: any agent's loader — or an image ENV /
	// run_args with no loader at all — injects git configuration the same
	// way, so every mountable agent pins it. The identity-source reads are
	// gemini's measured loader surface.
	shared := []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"}
	for _, agent := range []string{tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramGemini} {
		pins := credentialMountPinnedNames(agent, nil)
		for _, name := range shared {
			require.Contains(t, pins, name,
				"%s must be pinned empty on %s — count 0 / zero pairs makes loader-filled config inert", name, agent)
			_, denied := credentialMountDeniedNames(agent)[name]
			require.True(t, denied, "%s must also be refused to repository run_args on %s", name, agent)
		}
	}
	geminiPins := credentialMountPinnedNames(tmux.ProgramGemini, nil)
	for _, name := range []string{
		"GEMINI_CLI_USE_COMPUTE_ADC", "CLOUD_SHELL",
		"GCE_METADATA_HOST", "GCE_METADATA_IP",
		"DEBUG_AUTH", "DETECT_GCP_RETRIES", "METADATA_SERVER_DETECTION",
	} {
		require.Contains(t, geminiPins, name,
			"%s must be pinned empty for gemini — a chain link left unset is the one a .env claims", name)
		_, denied := credentialMountDeniedNames(tmux.ProgramGemini)[name]
		require.True(t, denied, "%s must be refused to repository run_args", name)
	}
	// The identity reads are gemini's surface — pinning them for every
	// agent would blank names those agents do not read for no gain.
	for _, agent := range []string{tmux.ProgramClaude, tmux.ProgramCodex} {
		require.NotContains(t, credentialMountPinnedNames(agent, nil), "GEMINI_CLI_USE_COMPUTE_ADC",
			"a gemini-specific identity selector is not another agent's to pin")
	}
	// An operator who forwards a name keeps their own value — the pin
	// yields to the explicit claim, same as every pinned name.
	forwarded := credentialMountPinnedNames(tmux.ProgramGemini, []string{"GIT_CONFIG_COUNT", "GCE_METADATA_HOST"})
	require.NotContains(t, forwarded, "GIT_CONFIG_COUNT")
	require.NotContains(t, forwarded, "GCE_METADATA_HOST")
}

// TestDockerCredentialRunArgs_RefusesGitConfigPairs is #7544's indexed-pair
// policy: a forwarded GIT_CONFIG_COUNT activates GIT_CONFIG_KEY_*/
// GIT_CONFIG_VALUE_* pairs, so the repository's run_args spelling of a pair
// is refused — the family stays af's or the operator's own.
func TestDockerCredentialRunArgs_RefusesGitConfigPairs(t *testing.T) {
	for _, name := range []string{"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_17", "GIT_CONFIG_VALUE_17"} {
		err := validateCredentialDockerRunArgs([]string{"-e", name + "=x"}, tmux.ProgramGemini, nil)
		require.Errorf(t, err, "%s in repository run_args must be refused — a forwarded count would activate it", name)
		require.Contains(t, err.Error(), name)
		require.Contains(t, err.Error(), "session_env_passthrough", "the refusal must name the operator remedy")
	}
}

// TestProvision_RefusesPaneBoundaryPassthrough is #7544's no-silent-override
// rule: the pane-boundary blank runs on EVERY docker session, so a
// session_env_passthrough entry naming one would be silently overridden —
// it is refused with a message instead.
func TestProvision_RefusesPaneBoundaryPassthrough(t *testing.T) {
	for _, name := range []string{
		"AF_SESSION", "AF_SESSION_GEN", "AF_HOME", "AF_INSTANCE_ID",
		"AF_TESTGUARD_RUN", "TMUX", "TMUX_PANE",
	} {
		runCalled, err, _ := provisionDockerCredentialGrantImageEnv(t, tmux.ProgramGemini,
			nil, []string{name}, nil)
		require.Falsef(t, runCalled, "%s must be refused before docker run — the blank would override it", name)
		require.Errorf(t, err, "%s must produce a refusal", name)
		require.Contains(t, err.Error(), name, "the refusal must name the variable")
		require.Contains(t, err.Error(), "session_env_passthrough", "the refusal must name the config knob to edit")
	}
}
