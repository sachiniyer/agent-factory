package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
	// The HOME chown is recursive so a non-root BYO image that ships root-owned
	// descendants under /root (e.g. /root/.codex or /root/.config with mode
	// 0700) does not leave them unreadable by the selected uid (#5424 review).
	// /workspace is freshly created by mkdir -p, so its chown stays shallow.
	require.Contains(t, chownJoined, "chown -R "+dockerNonRootImageUser+" "+shellQuote(dockerContainerHome),
		"prepareNonAccountUser must recursively chown HOME so root-owned descendants become accessible; call=%v", chown)
	require.NotContains(t, chownJoined, "chown -R "+dockerNonRootImageUser+" "+shellQuote(dockerWorkspaceDir),
		"/workspace is freshly created by mkdir -p; its chown must stay shallow; call=%v", chown)

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
// image and the round-trip integration test take) provisions exactly as it did
// before prepareNonAccountUser existed. The probe reports 0:0 and the step
// returns early — no chown runs and containerUser stays unset — so:
//
//   - Nothing chowns /root or /workspace. A docker.run_args bind mount there
//     attaches a HOST directory to the container, and a chown would re-own the
//     operator's host files (they persist after the container is reaped).
//   - Every provisioning exec (configureGit, cloneWorkspace, agent-server,
//     readBanner) runs as the image's default user with the container's own
//     env: no --user and no -e. A forced `-e HOME=/root` would override a HOME
//     the operator set via run_args (docker exec -e wins over the container's
//     configured env) — the second regression this early return prevents.
//
// The one exec that still carries --user is execSh's `chmod +x` of the copied
// `af` binary: docker cp lands it root-owned so only root can chmod it, which
// non-root images require — and on a root-default image running as 0:0 is the
// same user the image default resolves to anyway.
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

	var sawChmod bool
	for _, call := range calls {
		if len(call) == 0 || call[0] != "exec" {
			continue
		}
		joined := strings.Join(call, " ")
		require.NotContainsf(t, joined, "chown",
			"a root-default image must not be chown-prepared — a run_args bind mount at /root or /workspace would re-own host files; call=%v", call)
		require.NotContainsf(t, joined, "mkdir",
			"a root-default image must not be chown-prepared — the clone creates /workspace itself, as before; call=%v", call)
		if strings.Contains(joined, "chmod +x") {
			sawChmod = true
			require.Containsf(t, joined, "--user 0:0",
				"execSh's chmod must run as root — docker cp lands the binary root-owned; call=%v", call)
			continue
		}
		for _, arg := range call {
			require.NotEqualf(t, "--user", arg,
				"a root-default image must exec as the image's default user, not a forced uid (master behavior); call=%v", call)
			require.NotEqualf(t, "-e", arg,
				"a root-default image must not force -e HOME — a HOME the operator set via run_args must survive (docker exec -e would override it); call=%v", call)
			require.Falsef(t, strings.HasPrefix(arg, "HOME="),
				"a root-default image must not force HOME — a run_args HOME must survive; call=%v", call)
		}
	}
	require.True(t, sawChmod, "copyAfBinary's chmod must still run; calls=%v", calls)
}

// TestPrepareNonAccountUser_RootDefaultSkipsPreparation pins the unit contract
// behind the no-regression test above: when the id probe reports uid 0 the step
// performs no second exec (no mkdir/chown that could re-own a host directory
// bind-mounted at /root or /workspace) and leaves containerUser unset, so
// sessionExecOptions() returns nil and every later exec inherits the
// container's configured env — including a HOME the operator set via
// docker.run_args. `USER 0:1000` is a legal Dockerfile directive and probes as
// 0:1000, so the check is on the uid alone — a nonzero gid must not fall
// through into the chown path.
func TestPrepareNonAccountUser_RootDefaultSkipsPreparation(t *testing.T) {
	for _, probe := range []string{"0:0", "0:1000"} {
		t.Run("probe="+probe, func(t *testing.T) {
			var calls [][]string
			t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				return []byte(probe + "\n"), nil
			}))
			p := &dockerProvisioner{containerID: "cid"}

			require.NoError(t, p.prepareNonAccountUser())
			require.Len(t, calls, 1,
				"a uid-0 probe must be the only docker call — no chown/mkdir may run against a root-default image; calls=%v", calls)
			require.Contains(t, strings.Join(calls[0], " "), "id -u",
				"the single call must be the id probe; calls=%v", calls)
			require.Equal(t, "", p.containerUser,
				"containerUser must stay unset on a root-default image so sessionExecOptions adds no --user/-e HOME")
			require.Empty(t, p.sessionExecOptions(),
				"an unset containerUser must yield no exec overrides — execs inherit the container env, run_args HOME included")
		})
	}
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

// TestPrepareNonAccountUser_ChownsHomeRecursively pins that the HOME chown is
// recursive so a non-root BYO image that ships root-owned descendants under
// /root (e.g. /root/.codex or /root/.config with mode 0700) does not leave them
// unreadable by the selected uid — a shallow chown of the top-level directory
// alone would not fix the configured HOME for the agent (#5424 review).
// /workspace is freshly created by mkdir -p, so its chown stays shallow.
func TestPrepareNonAccountUser_ChownsHomeRecursively(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
			return []byte(dockerNonRootImageUser + "\n"), nil
		}
		return nil, nil
	}))
	p := &dockerProvisioner{containerID: "cid"}
	require.NoError(t, p.prepareNonAccountUser())

	chown := findExecCall(calls, "--user 0:0", "chown", dockerNonRootImageUser)
	require.NotNil(t, chown, "prepareNonAccountUser must chown HOME and /workspace as root; calls=%v", calls)
	joined := strings.Join(chown, " ")
	require.Contains(t, joined, "chown -R "+dockerNonRootImageUser+" "+shellQuote(dockerContainerHome),
		"HOME must be chowned recursively so root-owned descendants become accessible; call=%v", chown)
	require.NotContains(t, joined, "chown -R "+dockerNonRootImageUser+" "+shellQuote(dockerWorkspaceDir),
		"/workspace is freshly created by mkdir -p; its chown must stay shallow; call=%v", chown)
	require.Equal(t, dockerNonRootImageUser, p.containerUser,
		"containerUser must record the probed uid:gid so later execs run as it; calls=%v", calls)
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

// TestPrepareNonAccountUser_OwnerParseIsLocal pins the same contract as
// TestPrepareAccountUser_OwnerParseIsLocal: unparseable id output is af's
// failure (dockerOpLocal), never a dockerOpRootExec a full data root could
// attach an ENOSPC verdict to.
func TestPrepareNonAccountUser_OwnerParseIsLocal(t *testing.T) {
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
			return []byte("not-a-uid-gid\n"), nil
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})()
	p := &dockerProvisioner{containerID: "cid"}
	err := p.prepareNonAccountUser()
	require.Error(t, err)
	var se *dockerStepError
	require.True(t, errors.As(err, &se), "the owner-parse failure must carry an operation class")
	assert.Equal(t, dockerOpLocal, se.op,
		"unparseable id output is af's failure, never a filesystem verdict")
}

// TestPrepareNonAccountUser_OwnerReadCarriesDaemonWriteClass pins the same
// contract as TestPrepareAccountUser_OwnerReadCarriesDaemonWriteClass: the
// `id -u`/`id -g` probe is a read-only exec — an image missing `id` fails
// without attempting an unprivileged write. Its only data-root writes are
// dockerd's privileged exec bookkeeping, so the failure must carry the
// daemon-write class and never become eligible for the Bavail window.
func TestPrepareNonAccountUser_OwnerReadCarriesDaemonWriteClass(t *testing.T) {
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
			return nil, fmt.Errorf("exit status 127") // the image has no id
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	})()
	p := &dockerProvisioner{containerID: "cid"}
	err := p.prepareNonAccountUser()
	require.Error(t, err)
	var se *dockerStepError
	require.True(t, errors.As(err, &se), "the owner-read failure must carry an operation class")
	assert.Equal(t, dockerOpDaemonWrite, se.op,
		"a failed owner READ attempted no unprivileged write — it is dockerd bookkeeping, not a session writer")
}

// TestCredentialMountTargets pins the parser that extracts the in-container
// destinations of the read-only agent-credential bind mounts (#2194) that
// fall under the session home. Each `-v` entry is a "host:container:mode"
// pair; host and container are absolute unix paths with no colons, so the
// container destination is the segment between the last two colons. Only
// strict descendants of home are returned — credentials mount under
// /root/<rel> for a non-account session, never at /root itself.
func TestCredentialMountTargets(t *testing.T) {
	mounts := []string{
		"-v", "/host/.codex/auth.json:" + dockerContainerHome + "/.codex/auth.json:ro",
		"-v", "/host/.claude/.credentials.json:" + dockerContainerHome + "/.claude/.credentials.json:ro,z",
		// A mount NOT under home is dropped.
		"-v", "/host/other:/opt/other:ro",
	}
	targets := credentialMountTargets(mounts, dockerContainerHome)
	assert.Equal(t, []string{
		dockerContainerHome + "/.codex/auth.json",
		dockerContainerHome + "/.claude/.credentials.json",
	}, targets)

	// home itself is never a prune target: a credential never mounts at
	// /root, and pruning home would skip the whole chown.
	assert.Empty(t, credentialMountTargets(
		[]string{"-v", "/h:" + dockerContainerHome + ":ro"}, dockerContainerHome),
		"home itself must not be pruned — the chown must still chown it")
	assert.Empty(t, credentialMountTargets(nil, dockerContainerHome),
		"no mounts -> no targets")
}

// TestChownRecursiveScript pins the two shapes the recursive HOME chown
// takes: a bare `chown -R` when no bind mount lands under the home, and a
// `find -prune` that skips each read-only credential file (or a run_args bind)
// otherwise. The prune form chowns home itself and every other descendant so
// root-owned image config dirs (.codex, .config, …) become accessible, while
// the mounted credential file — read-only, already owned correctly on the host —
// is never touched (#5424 review). chown runs with -h so a symlink the image
// ships under /root is re-owned itself, not its target outside HOME.
func TestChownRecursiveScript(t *testing.T) {
	// No prune targets: the plain recursive chown is simpler and stays.
	assert.Equal(t, "chown -R 1000:1000 "+shellQuote(dockerContainerHome),
		chownRecursiveScript(dockerContainerHome, "1000:1000", nil))

	// With credential targets, find prunes them and chowns everything else.
	script := chownRecursiveScript(dockerContainerHome, "1000:1000", []string{
		dockerContainerHome + "/.codex/auth.json",
		dockerContainerHome + "/.claude/.credentials.json",
	})
	assert.Contains(t, script, "find "+shellQuote(dockerContainerHome),
		"the prune form walks home with find")
	assert.Contains(t, script, "-path "+shellQuote(dockerContainerHome+"/.codex/auth.json"),
		"the first credential target is a prune path")
	assert.Contains(t, script, "-path "+shellQuote(dockerContainerHome+"/.claude/.credentials.json"),
		"the second credential target is a prune path")
	assert.Contains(t, script, "-prune",
		"the credential targets are pruned, not chowned")
	assert.Contains(t, script, "-exec chown -h 1000:1000 {} +",
		"everything else under home is chowned to the probed uid, without dereferencing symlinks")
	assert.NotContains(t, script, "chown -R",
		"the prune form never uses chown -R, which would descend into the read-only mount")
}

// TestPrepareNonAccountUser_PrunesCredentialMountsFromRecursiveChown pins
// the fix for the read-only credential mount regression (#5424 review): when
// docker.mount_agent_credentials is on, the agent's credential file is
// bind-mounted read-only under /root at `docker run`, before this step. The
// recursive HOME chown must prune that mount — a `chown -R` that descends
// into it fails with EROFS and aborts provisioning before the agent can read
// its credential. The image-shipped parent dirs (.codex, .claude, …) are
// still chowned so the agent can read and update them, and /workspace stays a
// shallow chown because mkdir -p just created it.
func TestPrepareNonAccountUser_PrunesCredentialMountsFromRecursiveChown(t *testing.T) {
	credTarget := dockerContainerHome + "/.codex/auth.json"
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
			return []byte(dockerNonRootImageUser + "\n"), nil
		}
		return nil, nil
	}))
	p := &dockerProvisioner{
		containerID: "cid",
		credentialMounts: []string{
			"-v", "/host/.codex/auth.json:" + credTarget + ":ro",
		},
	}
	require.NoError(t, p.prepareNonAccountUser())

	chown := findExecCall(calls, "--user 0:0", "chown")
	require.NotNil(t, chown, "prepareNonAccountUser must chown HOME and /workspace as root; calls=%v", calls)
	joined := strings.Join(chown, " ")
	require.Contains(t, joined, "mkdir -p "+shellQuote(dockerContainerHome)+" "+shellQuote(dockerWorkspaceDir),
		"HOME and /workspace must be created; call=%v", chown)
	// The recursive HOME chown prunes the credential mount instead of a
	// bare chown -R, which would descend into the read-only bind and EROFS.
	require.Contains(t, joined, "find "+shellQuote(dockerContainerHome),
		"HOME chown uses find so the credential mount can be pruned; call=%v", chown)
	require.Contains(t, joined, "-path "+shellQuote(credTarget),
		"the credential mount target must be a prune path; call=%v", chown)
	require.Contains(t, joined, "-prune",
		"the credential mount must be pruned, not chowned; call=%v", chown)
	require.NotContains(t, joined, "chown -R "+dockerNonRootImageUser+" "+shellQuote(dockerContainerHome),
		"a bare recursive chown would EROFS the read-only credential mount; call=%v", chown)
	// /workspace is freshly created by mkdir -p, so its chown stays shallow.
	require.Contains(t, joined, "chown "+dockerNonRootImageUser+" "+shellQuote(dockerWorkspaceDir),
		"/workspace must still be shallow-chowned; call=%v", chown)
	require.NotContains(t, joined, "chown -R "+dockerNonRootImageUser+" "+shellQuote(dockerWorkspaceDir),
		"/workspace is freshly created; its chown must stay shallow; call=%v", chown)
	require.Equal(t, dockerNonRootImageUser, p.containerUser,
		"containerUser must record the probed uid:gid; calls=%v", calls)
}

// TestPrepareNonAccountUser_NoCredentialMount_RecursesPlainly pins that
// without a credential mount the HOME chown keeps its simple `chown -R`
// form — no find/prune overhead, and existing assertions about the recursive
// chown (TestPrepareNonAccountUser_ChownsHomeRecursively) still hold.
func TestPrepareNonAccountUser_NoCredentialMount_RecursesPlainly(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
			return []byte(dockerNonRootImageUser + "\n"), nil
		}
		return nil, nil
	}))
	p := &dockerProvisioner{containerID: "cid"} // no credentialMounts
	require.NoError(t, p.prepareNonAccountUser())
	chown := findExecCall(calls, "--user 0:0", "chown")
	require.NotNil(t, chown, "prepareNonAccountUser must chown as root; calls=%v", calls)
	joined := strings.Join(chown, " ")
	require.Contains(t, joined, "chown -R "+dockerNonRootImageUser+" "+shellQuote(dockerContainerHome),
		"with no credential mount the HOME chown stays a plain chown -R; call=%v", chown)
	require.NotContains(t, joined, "find",
		"find is only needed when a credential mount must be pruned; call=%v", chown)
}

// TestVolumeBindContainer pins the -v/--volume value parser that
// bindMountTargets uses to distinguish a host-source bind mount (whose
// recursive HOME chown must be pruned) from a named or anonymous volume
// (docker-managed, writable, left to the plain chown). The container
// destination is the segment between the last two colons, matching
// credentialMountTargets; only an absolute host source is a bind.
func TestVolumeBindContainer(t *testing.T) {
	// A bind mount: absolute host source, container under home, ro.
	c, ok := volumeBindContainer("/host/cache:" + dockerContainerHome + "/cache:ro")
	require.True(t, ok, "an absolute host source is a bind mount")
	assert.Equal(t, dockerContainerHome+"/cache", c)

	// The mode may carry a relabel suffix (":ro,z"); the container is still
	// the segment between the last two colons.
	c, ok = volumeBindContainer("/host/.codex:" + dockerContainerHome + "/.codex:ro,z")
	require.True(t, ok)
	assert.Equal(t, dockerContainerHome+"/.codex", c)

	// A named volume (name:container) is NOT a bind — the host source is not
	// an absolute path, so a chown into it is harmless and it is not pruned.
	_, ok = volumeBindContainer("myvol:" + dockerContainerHome + "/vol")
	assert.False(t, ok, "a named volume is docker-managed and not pruned")

	// An anonymous volume (bare container path, no colon) is not a bind.
	_, ok = volumeBindContainer(dockerContainerHome + "/vol")
	assert.False(t, ok, "an anonymous volume is not a bind")

	// An empty container destination is not a bind.
	_, ok = volumeBindContainer("/host::ro")
	assert.False(t, ok)
}

// TestMountBindContainer pins the --mount value parser: only type=bind with a
// destination under home is pruned, so a type=volume or type=tmpfs --mount is
// left to the plain recursive chown (it is docker-managed and writable).
func TestMountBindContainer(t *testing.T) {
	// A bind mount with a quoted dst is read the way Docker reads it.
	c, ok := mountBindContainer(`type=bind,src=/host/cache,dst=` + dockerContainerHome + `/cache`)
	require.True(t, ok)
	assert.Equal(t, dockerContainerHome+"/cache", c)

	// destination= and target= are the same as dst= (case-insensitive key).
	c, ok = mountBindContainer("type=bind,source=/host/x,target=" + dockerContainerHome + "/x")
	require.True(t, ok)
	assert.Equal(t, dockerContainerHome+"/x", c)

	c, ok = mountBindContainer("type=bind,DST=" + dockerContainerHome + "/y")
	require.True(t, ok, "DST is matched case-insensitively, the target is not")
	assert.Equal(t, dockerContainerHome+"/y", c)

	// A volume mount is docker-managed and not pruned.
	_, ok = mountBindContainer("type=volume,src=myvol,dst=" + dockerContainerHome + "/vol")
	assert.False(t, ok, "a volume mount is not a host bind")

	// A tmpfs mount is in-memory and not pruned.
	_, ok = mountBindContainer("type=tmpfs,dst=" + dockerContainerHome + "/tmp")
	assert.False(t, ok, "a tmpfs mount is not a host bind")
}

// TestBindMountTargets pins the run_args bind-mount extraction: every form
// that docker.run_args can use to install a host bind under the session home
// is pruned, while a bind outside home, a named/anonymous volume, and a tmpfs
// --mount are not.
func TestBindMountTargets(t *testing.T) {
	target := dockerContainerHome + "/cache"
	args := []string{
		// -v with a separate value.
		"-v", "/host/cache:" + target + ":ro",
		// --volume with an attached =value.
		"--volume=/host/x:" + dockerContainerHome + "/x",
		// --mount type=bind.
		"--mount", "type=bind,src=/host/y,dst=" + dockerContainerHome + "/y",
		// A mount NOT under home is dropped.
		"-v", "/host/other:/opt/other:ro",
		// A named volume under home is not a bind and is not pruned.
		"-v", "myvol:" + dockerContainerHome + "/vol",
		// An anonymous volume under home is not a bind.
		"-v", dockerContainerHome + "/anon",
		// A type=volume --mount under home is not pruned.
		"--mount", "type=volume,src=myvol,dst=" + dockerContainerHome + "/vol2",
	}
	got := bindMountTargets(args, dockerContainerHome)
	assert.Equal(t, []string{
		target,
		dockerContainerHome + "/x",
		dockerContainerHome + "/y",
	}, got, "only host-source binds under home are pruned; a named/anonymous volume is not a bind")

	// Docker Engine 23+ accepts a relative host path as a bind source; it is a
	// host bind, not a named volume, and must be pruned like an absolute one.
	got = bindMountTargets([]string{
		"-v", "./cache:" + target,
		"-v", "../up:" + dockerContainerHome + "/up:ro",
		"-v", ".:" + dockerContainerHome + "/dot",
		"-v", "cache:" + dockerContainerHome + "/named",
	}, dockerContainerHome)
	assert.Equal(t, []string{target, dockerContainerHome + "/up", dockerContainerHome + "/dot"}, got,
		"relative host-path sources are binds; a bare name is a volume")

	// A -v hidden behind another option in a combined short option (e.g. -tv)
	// is still read by the pflag walk the run_args validator uses, so a bind
	// mounted that way is pruned too.
	got = bindMountTargets([]string{"-tv", "/host/cache:" + target + ":ro"}, dockerContainerHome)
	assert.Equal(t, []string{target}, got,
		"a -v behind a boolean option in a combined short option is still pruned")

	// home itself is never a prune target: a bind over /root skips the whole
	// chown, and is a different configuration af does not support pruning.
	assert.Empty(t, bindMountTargets([]string{"-v", "/host:" + dockerContainerHome + ":ro"}, dockerContainerHome),
		"home itself must not be pruned — the chown must still chown it")
	assert.Empty(t, bindMountTargets(nil, dockerContainerHome),
		"no run_args -> no targets")
}

// TestChownRecursiveScript_PrunesWithChownH pins that the find form uses
// `chown -h` so a symlink the image ships under /root is re-owned itself
// rather than its target outside HOME, while a plain `chown` would
// dereference a command-line symlink and re-own the referent (#5424 review).
func TestChownRecursiveScript_PrunesWithChownH(t *testing.T) {
	script := chownRecursiveScript(dockerContainerHome, "1000:1000",
		[]string{dockerContainerHome + "/.codex/auth.json"})
	assert.Contains(t, script, "-exec chown -h 1000:1000 {} +",
		"the prune form chowns with -h so symlinks are not dereferenced")
	assert.NotContains(t, script, "chown -R",
		"the prune form never uses chown -R")
}

// TestPrepareNonAccountUser_PrunesRunArgsBindMounts pins that a docker.run_args
// bind beneath the session home is pruned from the recursive HOME chown, so a
// read-write bind is not re-owned to the container uid and a read-only bind
// does not EROFS-abort provisioning — symmetric with the credential-mount
// prune (#5424 review). A named or anonymous volume in run_args is NOT pruned,
// because a chown into a docker-managed volume is harmless.
func TestPrepareNonAccountUser_PrunesRunArgsBindMounts(t *testing.T) {
	bindTarget := dockerContainerHome + "/cache"
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
			return []byte(dockerNonRootImageUser + "\n"), nil
		}
		return nil, nil
	}))
	p := &dockerProvisioner{
		containerID: "cid",
		runArgs: []string{
			"-v", "/host/cache:" + bindTarget + ":ro",
			"--volume=/host/x:" + dockerContainerHome + "/x",
			"--mount", "type=bind,src=/host/y,dst=" + dockerContainerHome + "/y",
			// A named volume under home is docker-managed and stays in the chown.
			"-v", "myvol:" + dockerContainerHome + "/vol",
			// A bind NOT under home is not pruned.
			"-v", "/host/other:/opt/other:ro",
		},
	}
	require.NoError(t, p.prepareNonAccountUser())

	chown := findExecCall(calls, "--user 0:0", "chown")
	require.NotNil(t, chown, "prepareNonAccountUser must chown HOME and /workspace as root; calls=%v", calls)
	joined := strings.Join(chown, " ")
	require.Contains(t, joined, "find "+shellQuote(dockerContainerHome),
		"a run_args bind under home makes the HOME chown use find so it can be pruned; call=%v", chown)
	for _, prune := range []string{bindTarget, dockerContainerHome + "/x", dockerContainerHome + "/y"} {
		require.Contains(t, joined, "-path "+shellQuote(prune),
			"each run_args bind under home must be a prune path; call=%v", chown)
	}
	require.Contains(t, joined, "-prune",
		"the run_args binds must be pruned, not chowned; call=%v", chown)
	require.Contains(t, joined, "-exec chown -h "+dockerNonRootImageUser+" {} +",
		"the prune form chowns with -h so symlinks are not dereferenced; call=%v", chown)
	// A named volume under home is NOT pruned — it is docker-managed and a
	// chown into it is harmless — so it must not appear as a -path in the
	// prune list.
	require.NotContains(t, joined, "-path "+shellQuote(dockerContainerHome+"/vol"),
		"a named volume under home is not a bind and must not be pruned; call=%v", chown)
	require.NotContains(t, joined, "-path "+shellQuote(dockerContainerHome+"/anon"),
		"an anonymous volume is not a bind and must not be pruned; call=%v", chown)
	// /workspace stays a shallow chown.
	require.Contains(t, joined, "chown "+dockerNonRootImageUser+" "+shellQuote(dockerWorkspaceDir),
		"/workspace must still be shallow-chowned; call=%v", chown)
	require.Equal(t, dockerNonRootImageUser, p.containerUser,
		"containerUser must record the probed uid:gid; calls=%v", calls)
}

// TestPrepareNonAccountUser_PrunesCredentialAndRunArgsBindTogether pins that
// the credential mount and a run_args bind under home are both pruned, and a
// run_args bind that coincides with the credential mount is not duplicated.
func TestPrepareNonAccountUser_PrunesCredentialAndRunArgsBindTogether(t *testing.T) {
	credTarget := dockerContainerHome + "/.codex/auth.json"
	runTarget := dockerContainerHome + "/cache"
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
			return []byte(dockerNonRootImageUser + "\n"), nil
		}
		return nil, nil
	}))
	p := &dockerProvisioner{
		containerID: "cid",
		credentialMounts: []string{
			"-v", "/host/.codex/auth.json:" + credTarget + ":ro",
		},
		runArgs: []string{
			"-v", "/host/cache:" + runTarget + ":rw",
			// The same path as the credential mount — must not be duplicated.
			"-v", "/host/dup:" + credTarget + ":ro",
		},
	}
	require.NoError(t, p.prepareNonAccountUser())

	chown := findExecCall(calls, "--user 0:0", "chown")
	require.NotNil(t, chown, "prepareNonAccountUser must chown as root; calls=%v", calls)
	joined := strings.Join(chown, " ")
	require.Contains(t, joined, "-path "+shellQuote(credTarget),
		"the credential mount target must be a prune path; call=%v", chown)
	require.Contains(t, joined, "-path "+shellQuote(runTarget),
		"the run_args bind target must be a prune path; call=%v", chown)
	// The duplicated credential target (also in runArgs) must appear exactly
	// once in the prune list.
	count := strings.Count(joined, "-path "+shellQuote(credTarget))
	require.Equal(t, 1, count, "a path in both credentialMounts and runArgs is deduped; call=%v", chown)
}
