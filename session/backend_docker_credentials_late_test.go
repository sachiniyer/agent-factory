package session

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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
