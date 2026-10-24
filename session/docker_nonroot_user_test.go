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
)

// dockerNonAccountProvisionFixture wires the hermetic preconditions a non-account
// docker provision needs to reach runContainer (a local engine so the pre-run
// locality guard passes without a real docker call, docker "found" on PATH, a
// stub `af` binary to copy in), and returns the repo whose .agent-factory
// config selects the docker backend with the given image. Per-call docker
// behavior is injected by each test via SetDockerExecForTest.
func dockerNonAccountProvisionFixture(t *testing.T, image string) string {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))
	repo := initTempGitRepo(t)
	runGit(t, repo, "remote", "add", "origin", "https://example.invalid/fixture.git")
	writeInRepoConfig(t, repo, map[string]any{
		"backend": "docker",
		"docker":  map[string]any{"image": image},
	})
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	t.Cleanup(SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af")))
	return repo
}

// dockerNonRootImageUser is the uid:gid the fake non-root BYO image reports
// when probed — a USER directive resolving to a non-root uid, the scenario
// the bug report reproduces.
const dockerNonRootImageUser = "1000:1000"

// fakeNonRootImageDockerAnswers drives a non-account provision to completion
// against a fake docker CLI whose image defaults to imageUser (uid:gid). It
// records every docker invocation into calls so the test can assert the
// provisioning execs run as the right user with the right HOME.
func fakeNonRootImageDockerAnswers(t *testing.T, calls *[][]string, imageUser string) func(context.Context, []string, ...string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		switch args[0] {
		case "info":
			return []byte("non-root-image-engine\n"), nil
		case "run":
			return []byte(dockerCreatedID + "\n"), nil
		case "cp", "rm":
			return nil, nil
		case "port":
			return []byte("127.0.0.1:49152\n"), nil
		case "exec":
			switch {
			case strings.Contains(joined, "id -u"):
				// prepareNonAccountUser's probe of the image's default USER.
				return []byte(imageUser + "\n"), nil
			case strings.Contains(joined, "cat "+dockerBannerPath):
				return []byte(`{"addr":":8000","token":"test-only","title":"non-root-image"}`), nil
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	}
}

// findExecCall returns the first recorded docker call that begins with "exec"
// and whose joined argv contains all of the given substrings, or nil.
func findExecCall(calls [][]string, contains ...string) []string {
	for _, call := range calls {
		if len(call) == 0 || call[0] != "exec" {
			continue
		}
		joined := strings.Join(call, " ")
		match := true
		for _, sub := range contains {
			if !strings.Contains(joined, sub) {
				match = false
				break
			}
		}
		if match {
			return call
		}
	}
	return nil
}

// TestDockerProvision_NonAccount_NonRootImage_Provisions asserts the central
// guarantee of the fix: a non-account session on a BYO image whose USER is
// non-root provisions end-to-end. Before the fix every provisioning exec ran
// as the image's default (non-root) user while HOME was forced to /root
// (root-owned, mode 0700), so the first `git config --global` failed with
// "could not lock config file /root/.gitconfig: Permission denied" and the
// session was rejected at create time.
//
// After the fix prepareNonAccountUser probes the image's default USER, chowns
// HOME and /workspace to that uid as root (0:0), records the uid in
// containerUser, and every subsequent exec (configureGit, cloneWorkspace,
// agent-server, readBanner) runs as that user via sessionExecOptions with
// HOME=/root (now writable). The chmod of the copied `af` binary runs as
// 0:0 because `docker cp` lands it root-owned.
func TestDockerProvision_NonAccount_NonRootImage_Provisions(t *testing.T) {
	repo := dockerNonAccountProvisionFixture(t, "nonroot/image:latest")
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(fakeNonRootImageDockerAnswers(t, &calls, dockerNonRootImageUser)))

	_, err := dockerRuntime{}.Provision(ProvisionSpec{
		RepoRoot: repo,
		Title:    "non-root-image",
		CloneURL: "file:///fixture.git",
	})
	require.NoError(t, err, "a non-account session on a non-root-USER BYO image must provision; calls=%v", calls)

	// The probe runs as the IMAGE's default user (no --user override), so a
	// non-root USER actually surfaces as a non-root uid. A --user on the probe
	// would silently report 0:0 and hide the non-root condition.
	probe := findExecCall(calls, "id -u")
	require.NotNil(t, probe, "prepareNonAccountUser must probe the image's default USER; calls=%v", calls)
	for _, arg := range probe {
		require.NotEqual(t, "--user", arg,
			"the default-USER probe must run as the image's default user (no --user), not as root; call=%v", probe)
	}

	// The chown of HOME and /workspace runs as root (0:0): only root can chown
	// to an arbitrary uid, and these are root-owned paths on a fresh container.
	chown := findExecCall(calls, "--user 0:0", "chown", dockerNonRootImageUser)
	require.NotNil(t, chown, "prepareNonAccountUser must chown HOME and /workspace to the image user as root; calls=%v", calls)
	chownJoined := strings.Join(chown, " ")
	require.Contains(t, chownJoined, "mkdir -p",
		"prepareNonAccountUser must create /workspace for the clone; call=%v", chown)
	require.Contains(t, chownJoined, shellQuote(dockerWorkspaceDir),
		"prepareNonAccountUser must create /workspace for the clone; call=%v", chown)
	require.Contains(t, chownJoined, "chown "+dockerNonRootImageUser,
		"prepareNonAccountUser must chown to the probed uid:gid; call=%v", chown)
	require.Contains(t, chownJoined, shellQuote(dockerContainerHome),
		"prepareNonAccountUser must chown HOME (dockerContainerHome) so the non-root user can write it; call=%v", chown)
	require.Contains(t, chownJoined, shellQuote(dockerWorkspaceDir),
		"prepareNonAccountUser must chown /workspace so the non-root user can clone into it; call=%v", chown)

	// git config (configureGit) runs as the probed non-root user with HOME=/root
	// (now writable because it was chowned). Before the fix this ran as the
	// image default user against a root-owned /root and failed.
	gitConfig := findExecCall(calls, "--user "+dockerNonRootImageUser, "git config --global")
	require.NotNil(t, gitConfig, "configureGit must run as the image's default user; calls=%v", calls)
	require.Contains(t, strings.Join(gitConfig, " "), "HOME="+dockerContainerHome,
		"configureGit keeps HOME=/root (now writable); call=%v", gitConfig)

	// git clone (cloneWorkspace) runs as the probed non-root user with HOME=/root.
	gitClone := findExecCall(calls, "--user "+dockerNonRootImageUser, "git clone")
	require.NotNil(t, gitClone, "cloneWorkspace must run as the image's default user; calls=%v", calls)
	require.Contains(t, strings.Join(gitClone, " "), "HOME="+dockerContainerHome,
		"cloneWorkspace keeps HOME=/root; call=%v", gitClone)
	require.Contains(t, strings.Join(gitClone, " "), shellQuote(dockerWorkspaceDir),
		"cloneWorkspace clones into /workspace; call=%v", gitClone)

	// chmod of the copied `af` binary runs as root (0:0): docker cp lands the
	// binary root-owned, and a non-root user cannot chmod a root-owned file.
	chmod := findExecCall(calls, "--user 0:0", "chmod +x")
	require.NotNil(t, chmod, "copyAfBinary's chmod must run as root; calls=%v", calls)

	// The detached agent-server runs as the image's default user with HOME=/root
	// (the same writable security context that wrote the banner), not as root
	// and not as a bare image-default exec with no --user.
	serverStart := findExecCall(calls, "exec -d", "--user "+dockerNonRootImageUser, "agent-server")
	require.NotNil(t, serverStart, "startAgentServer must run as the image's default user; calls=%v", calls)
	require.Contains(t, strings.Join(serverStart, " "), "HOME="+dockerContainerHome,
		"startAgentServer keeps HOME=/root; call=%v", serverStart)

	// readBanner runs as the SAME image-default user that wrote the banner, so
	// a banner file written with a restrictive umask stays readable to its
	// writer (the account-path contract, now mirrored on the non-account path).
	bannerRead := findExecCall(calls, "--user "+dockerNonRootImageUser, "cat "+dockerBannerPath)
	require.NotNil(t, bannerRead, "readBanner must run as the image's default user; calls=%v", calls)
	require.Contains(t, strings.Join(bannerRead, " "), "HOME="+dockerContainerHome,
		"readBanner keeps HOME=/root; call=%v", bannerRead)
}

// TestDockerProvision_NonAccount_RootDefaultImage_IsUnchanged pins the no-
// regression half: a root-default BYO image (the path every shipped/tested
// image and the round-trip integration test take) provisions exactly as
// before. The probe reports 0:0, the chown is a no-op, every provisioning exec
// runs as 0:0 with HOME=/root, and the agent-server runs as 0:0 with HOME=/root,
// so the credential-mount anchors under /root stay where agents expect them.
func TestDockerProvision_NonAccount_RootDefaultImage_IsUnchanged(t *testing.T) {
	repo := dockerNonAccountProvisionFixture(t, "rootdefault/image:latest")
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(fakeNonRootImageDockerAnswers(t, &calls, "0:0")))

	_, err := dockerRuntime{}.Provision(ProvisionSpec{
		RepoRoot: repo,
		Title:    "root-default-image",
		CloneURL: "file:///fixture.git",
	})
	require.NoError(t, err, "a non-account session on a root-default image must provision; calls=%v", calls)

	// The probe reports 0:0 and every subsequent exec runs as 0:0 with HOME=/root.
	for _, call := range calls {
		if len(call) == 0 || call[0] != "exec" {
			continue
		}
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "id -u") {
			continue // the probe itself carries no --user by design
		}
		require.Containsf(t, joined, "--user 0:0",
			"a root-default image must run every exec as 0:0 (same as the image default); call=%v", call)
		require.NotContainsf(t, joined, "HOME=/af-home",
			"a root-default non-account session keeps HOME=/root (no /af-home leak from the account path); call=%v", call)
	}
	// HOME stays /root: the credential-mount anchors and the doc's
	// "Container-native HOME and PATH remain owned by the image" claim hold.
	bannerRead := findExecCall(calls, "cat "+dockerBannerPath)
	require.NotNil(t, bannerRead)
	require.Contains(t, strings.Join(bannerRead, " "), "HOME="+dockerContainerHome)
}

// TestDockerProvision_NonAccount_ProbeFailure_RefusesCreate pins the fail-
// closed posture: if the image's default USER cannot be probed (docker exec
// returns an error), provisioning refuses at create time rather than falling
// back to a silent assumption that would reproduce the original bug. The
// diagnostic names the probe step so an operator sees the real cause, not a
// downstream `git config` permission error.
func TestDockerProvision_NonAccount_ProbeFailure_RefusesCreate(t *testing.T) {
	repo := dockerNonAccountProvisionFixture(t, "broken/image:latest")
	probeErr := fmt.Errorf("exit status 1")
	var sawChown bool
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		switch args[0] {
		case "info":
			return []byte("broken-image-engine\n"), nil
		case "run":
			return []byte(dockerCreatedID + "\n"), nil
		case "rm":
			return []byte(dockerCreatedID + "\n"), nil
		case "exec":
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "id -u") {
				return []byte("docker: no such container\n"), probeErr
			}
			if strings.Contains(joined, "chown") {
				sawChown = true
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	}))

	_, err := dockerRuntime{}.Provision(ProvisionSpec{
		RepoRoot: repo,
		Title:    "broken-image",
		CloneURL: "file:///fixture.git",
	})
	require.Error(t, err, "a probe failure must refuse the create rather than proceed against an unknown default user")
	require.Contains(t, err.Error(), "image's default user",
		"the diagnostic must name the probe step so an operator sees the real cause, not a downstream git error")
	require.False(t, sawChown, "a probe failure must not proceed to chown or any later provisioning step")
}

// TestPrepareNonAccountUser_NoOpForAccount pins the dispatch: an account-
// scoped session never reaches prepareNonAccountUser (the account path has its
// own owner probe via the mounted directory). A non-account probe running
// during an account provision would race the account's own containerUser
// assignment and silently replace it with the image's default USER.
func TestPrepareNonAccountUser_NoOpForAccount(t *testing.T) {
	p := &dockerProvisioner{spec: ProvisionSpec{Account: sessionenv.Account{Dir: "/af-account", Name: "test", Agent: "codex"}}}
	require.NoError(t, p.prepareNonAccountUser(),
		"prepareNonAccountUser must be a no-op for an account-scoped session")
	require.Equal(t, "", p.containerUser,
		"the non-account probe must not overwrite the account path's containerUser")
}
