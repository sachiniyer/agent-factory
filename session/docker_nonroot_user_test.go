package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
			case strings.Contains(joined, "/proc/self/mountinfo"):
				return mountTableAnswer(), nil
			case strings.Contains(joined, "cat "+dockerBannerPath):
				return []byte(`{"addr":":8000","token":"test-only","title":"non-root-image"}`), nil
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	}
}

// mountinfoLine renders one /proc/self/mountinfo line for mountPoint, escaping
// space/tab/newline/backslash in octal the way the kernel does.
func mountinfoLine(id int, mountPoint, fstype string) string {
	esc := strings.NewReplacer(" ", `\040`, "\t", `\011`, "\n", `\012`, `\`, `\134`).Replace(mountPoint)
	return fmt.Sprintf("%d 1 0:%d / %s rw,relatime - %s src rw", id, id, esc, fstype)
}

// mountTableAnswer is the fake container's answer to readMountTable's exec: the
// resolved home and workspace, then a mountinfo with a root mount plus
// mountPoints.
func mountTableAnswer(mountPoints ...string) []byte {
	lines := []string{dockerContainerHome, dockerWorkspaceDir, mountinfoLine(1, "/", "overlay")}
	for i, mp := range mountPoints {
		lines = append(lines, mountinfoLine(10+i, mp, "ext4"))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func isMountTableExec(args []string) bool {
	return len(args) > 0 && args[0] == "exec" && strings.Contains(strings.Join(args, " "), "/proc/self/mountinfo")
}

// nonRootDockerStub answers the id probe with the non-root image user and the
// mount-table read with mountPoints, recording every call.
func nonRootDockerStub(calls *[][]string, mountPoints ...string) func(context.Context, []string, ...string) ([]byte, error) {
	return func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
			return []byte(dockerNonRootImageUser + "\n"), nil
		}
		if isMountTableExec(args) {
			return mountTableAnswer(mountPoints...), nil
		}
		return nil, nil
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
	t.Cleanup(SetDockerExecForTest(nonRootDockerStub(&calls)))
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

// TestChownRecursiveScript_PruneMatchesMetacharactersLiterally runs the generated
// find expression's prune half against a real tree: find's -path takes a shell
// pattern, so /cache[1] must not match /cache1 and must match itself (#5424
// review).
func TestChownRecursiveScript_PruneMatchesMetacharactersLiterally(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"cache[1]", "cache1", "we?rd*", "weird"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d, "inner"), 0o755))
	}
	prune := []string{root + "/cache[1]/", root + "/we?rd*"}
	script := chownRecursiveScript(root, "0:0", prune)
	// Swap the chown for echo so the test needs no privilege and just lists what
	// the expression would have chowned.
	script = strings.Replace(script, "-exec chown -h 0:0 {} +", "-print", 1)
	out, err := exec.Command("sh", "-c", script).Output()
	require.NoError(t, err)
	listed := string(out)
	assert.NotContains(t, listed, "cache[1]", "the literal bracketed mount and its subtree are pruned")
	assert.NotContains(t, listed, "we?rd*", "the literal ?/* mount is pruned")
	assert.Contains(t, listed, filepath.Join(root, "cache1", "inner"), "a pattern-lookalike sibling is still chowned")
	assert.Contains(t, listed, filepath.Join(root, "weird", "inner"), "a pattern-lookalike sibling is still chowned")
}

// TestPrepareNonAccountUser_PrunesEveryMountUnderHome is the property behind
// the #5424 rounds: whatever created a mount under HOME — -v, --mount,
// --volumes-from, --tmpfs, an image-declared VOLUME, af's read-only credential
// bind — the kernel's mount table lists it, so the recursive chown never crosses
// into it, with no knowledge of how docker run was spelled.
func TestPrepareNonAccountUser_PrunesEveryMountUnderHome(t *testing.T) {
	mounts := []string{
		dockerContainerHome + "/.codex/auth.json", // af credential bind (single file, ro)
		dockerContainerHome + "/cache",            // -v /host:/root/cache
		dockerContainerHome + "/shared",           // --volumes-from donor's bind
		dockerContainerHome + "/data",             // image-declared VOLUME
		dockerContainerHome + "/scratch",          // --tmpfs
		dockerContainerHome + "/odd dir/x[1]*",    // spaces and find metacharacters
		"/opt/elsewhere",                          // not under HOME: not pruned
	}
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(nonRootDockerStub(&calls, mounts...)))
	p := &dockerProvisioner{containerID: "cid"}
	require.NoError(t, p.prepareNonAccountUser())

	chown := findExecCall(calls, "--user 0:0", "-prune")
	require.NotNil(t, chown, "a mount under HOME must switch the chown to the pruning form; calls=%v", calls)
	joined := strings.Join(chown, " ")
	for _, m := range mounts[:6] {
		assert.Contains(t, joined, "-path "+shellQuote(findLiteralPattern(m)), "mount %q must be pruned", m)
	}
	assert.NotContains(t, joined, "/opt/elsewhere", "a mount outside HOME is irrelevant to the HOME chown")
	assert.NotContains(t, joined, "chown -R", "a recursive chown would cross into the mounts")
	assert.Contains(t, joined, "-exec chown -h "+dockerNonRootImageUser)
	assert.Contains(t, joined, "chown "+dockerNonRootImageUser+" "+shellQuote(dockerWorkspaceDir))
}

func TestPrepareNonAccountUser_NoMountUnderHome_RecursesPlainly(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(nonRootDockerStub(&calls, "/opt/elsewhere", "/proc")))
	p := &dockerProvisioner{containerID: "cid"}
	require.NoError(t, p.prepareNonAccountUser())
	joined := strings.Join(findExecCall(calls, "--user 0:0", "chown"), " ")
	assert.Contains(t, joined, "chown -R "+dockerNonRootImageUser+" "+shellQuote(dockerContainerHome))
	assert.NotContains(t, joined, "find")
}

// Fail closed: with no trustworthy mount table the chown is refused, never
// guessed — a missing prune set would let a recursive chown cross a host mount.
func TestPrepareNonAccountUser_UnreadableMountTableRefusesTheChown(t *testing.T) {
	for name, answer := range map[string]func() ([]byte, error){
		"exec fails":     func() ([]byte, error) { return []byte("sh: cat: not found"), errors.New("exit status 127") },
		"empty answer":   func() ([]byte, error) { return nil, nil },
		"garbage answer": func() ([]byte, error) { return []byte("/root\n/workspace\nnot a mount table\n"), nil },
		"no root mount": func() ([]byte, error) {
			return []byte("/root\n/workspace\n" + mountinfoLine(2, "/proc", "proc") + "\n"), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
					return []byte(dockerNonRootImageUser + "\n"), nil
				}
				if isMountTableExec(args) {
					return answer()
				}
				return nil, nil
			}))
			p := &dockerProvisioner{containerID: "cid"}
			err := p.prepareNonAccountUser()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "mount table")
			assert.Nil(t, findExecCall(calls, "chown"), "no chown may run without a usable mount table; calls=%v", calls)
			assert.Empty(t, p.containerUser, "the user must not be recorded when the chown was refused")
		})
	}
}

func TestMountPointHelpers(t *testing.T) {
	table := strings.Join([]string{
		mountinfoLine(1, "/", "overlay"),
		mountinfoLine(2, "/root/cache", "ext4"),
		mountinfoLine(3, "/root/with space/x[1]", "ext4"),
		mountinfoLine(4, "/rootfs-lookalike", "ext4"), // shares the prefix, not under /root
		mountinfoLine(5, "/root/cache", "ext4"),       // duplicate (stacked mount)
		mountinfoLine(6, "/opt/other", "ext4"),
		mountinfoLine(7, "/root/tmp/", "tmpfs"),
		mountinfoLine(8, "/data", "ext4"),
	}, "\n")
	points, err := parseMountPoints(table)
	require.NoError(t, err)
	assert.Equal(t, []string{"/root/cache", "/root/with space/x[1]", "/root/tmp"}, mountPointsUnder(points, "/root"),
		"every mount strictly beneath HOME, decoded and de-duplicated; a sibling sharing the prefix is not beneath it")

	assert.Equal(t, "/", enclosingMount(points, "/root"), "a directory on the container's own filesystem is enclosed by the root mount")
	assert.Equal(t, "/data", enclosingMount(points, "/data/home"), "a directory beneath a mount is enclosed by it")
	assert.Equal(t, "/data", enclosingMount(points, "/data"), "a mount point is enclosed by itself")
	assert.Equal(t, "/root/cache", enclosingMount(points, "/root/cache/deep/er"))
	assert.Equal(t, "/", enclosingMount(points, "/rootfs-lookalike2"), "a sibling sharing a mount's name prefix is not inside it")

	for name, bad := range map[string]string{
		"empty":          "",
		"no root mount":  mountinfoLine(2, "/root/cache", "ext4"),
		"short line":     mountinfoLine(1, "/", "overlay") + "\n36 35 98:0",
		"relative mount": mountinfoLine(1, "/", "overlay") + "\n9 1 0:9 / rel rw - ext4 s rw",
	} {
		_, err := parseMountPoints(bad)
		assert.Error(t, err, name+": an unusable mount table must be an error, never an empty prune set")
	}
}

// HOME or the workspace AT or BENEATH any mount other than the container's own
// root filesystem — a host bind however it was created — is not af's to re-own:
// nothing is chowned there, and the reason is logged (#5424 review).
func TestPrepareNonAccountUser_HomeInsideAnEnclosingMountIsNeverChowned(t *testing.T) {
	t.Run("home symlinked into a bind-mounted parent", func(t *testing.T) {
		var calls [][]string
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
				return []byte(dockerNonRootImageUser + "\n"), nil
			}
			if isMountTableExec(args) {
				// /root -> /data/home, with a host bind at /data; workspace is on the root fs.
				return []byte("/data/home\n" + dockerWorkspaceDir + "\n" +
					mountinfoLine(1, "/", "overlay") + "\n" + mountinfoLine(2, "/data", "ext4") + "\n"), nil
			}
			return nil, nil
		}))
		p := &dockerProvisioner{containerID: "cid"}
		require.NoError(t, p.prepareNonAccountUser())
		joined := strings.Join(findExecCall(calls, "--user 0:0", "mkdir -p "+shellQuote(dockerContainerHome)), " ")
		assert.NotContains(t, joined, "chown -R", "HOME is inside a host bind: no recursive chown")
		assert.NotContains(t, joined, "find ", "HOME is inside a host bind: no chown of any kind")
		assert.NotContains(t, joined, "/data/home")
		assert.Contains(t, joined, "chown "+dockerNonRootImageUser+" "+shellQuote(dockerWorkspaceDir),
			"the workspace is on the container's root filesystem and is still chowned")
	})
	t.Run("home and workspace are themselves mounts", func(t *testing.T) {
		var calls [][]string
		t.Cleanup(SetDockerExecForTest(nonRootDockerStub(&calls, dockerContainerHome, dockerWorkspaceDir)))
		p := &dockerProvisioner{containerID: "cid"}
		require.NoError(t, p.prepareNonAccountUser())
		joined := strings.Join(findExecCall(calls, "--user 0:0", "mkdir -p "+shellQuote(dockerContainerHome)), " ")
		assert.NotContains(t, joined, "chown", "a bind AT home/workspace is the operator's directory: nothing is re-owned")
	})
	t.Run("workspace beneath a bind", func(t *testing.T) {
		var calls [][]string
		t.Cleanup(SetDockerExecForTest(nonRootDockerStub(&calls, "/workspace")))
		p := &dockerProvisioner{containerID: "cid"}
		require.NoError(t, p.prepareNonAccountUser())
		joined := strings.Join(findExecCall(calls, "--user 0:0", "mkdir -p "+shellQuote(dockerContainerHome)), " ")
		assert.Contains(t, joined, "chown -R "+dockerNonRootImageUser+" "+shellQuote(dockerContainerHome))
		assert.NotContains(t, joined, " chown "+dockerNonRootImageUser+" "+shellQuote(dockerWorkspaceDir))
	})
}

// #5950: a HOME that resolves to "/" (or a system directory) is refused before
// any chown runs — the recursive chown must never widen to the container root.
func TestPrepareNonAccountUser_UnsafeResolvedHomeIsRefusedBeforeAnyChown(t *testing.T) {
	for _, home := range []string{"/", "/usr", "/etc", "/home", "/var"} {
		t.Run(home, func(t *testing.T) {
			var calls [][]string
			t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
					return []byte(dockerNonRootImageUser + "\n"), nil
				}
				if isMountTableExec(args) {
					return []byte(home + "\n" + dockerWorkspaceDir + "\n" +
						mountinfoLine(1, "/", "overlay") + "\n" + mountinfoLine(2, "/data", "ext4") + "\n"), nil
				}
				return nil, nil
			}))
			p := &dockerProvisioner{containerID: "cid"}
			err := p.prepareNonAccountUser()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "refusing to chown")
			assert.Contains(t, err.Error(), "HOME")
			for _, c := range calls {
				assert.NotContains(t, strings.Join(c, " "), "chown", "no chown of any form may run: %v", c)
			}
			assert.Empty(t, p.containerUser, "a refused provisioning must not switch the exec user")
		})
	}
}

func TestPrepareNonAccountUser_WorkspaceResolvingToRootIsRefused(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
			return []byte(dockerNonRootImageUser + "\n"), nil
		}
		if isMountTableExec(args) {
			return []byte(dockerContainerHome + "\n/\n" + mountinfoLine(1, "/", "overlay") + "\n"), nil
		}
		return nil, nil
	}))
	p := &dockerProvisioner{containerID: "cid"}
	require.Error(t, p.prepareNonAccountUser())
	for _, c := range calls {
		assert.NotContains(t, strings.Join(c, " "), "chown", "%v", c)
	}
}

func TestUnchownableHome(t *testing.T) {
	assert.NoError(t, unchownableHome("/root"))
	assert.NoError(t, unchownableHome("/home/dev"))
	assert.NoError(t, unchownableHome("/opt/app/home"))
	for _, bad := range []string{"/", "//", "/usr", "/usr/", "/etc"} {
		assert.Error(t, unchownableHome(bad), bad)
	}
}

// A "/" root must report every other mount beneath it (the old prefix check
// built "//" and matched none).
func TestMountPointsUnderRootFindsEveryMount(t *testing.T) {
	assert.ElementsMatch(t, []string{"/data", "/root/x"}, mountPointsUnder([]string{"/", "/data", "/root/x"}, "/"))
}

// #5951: every mount kind under HOME is pruned — read-only named volumes, rw
// named volumes and read-only binds alike — because the prune set is the whole
// mount table, not the bind mounts.
func TestPrepareNonAccountUser_PrunesReadOnlyAndNamedVolumesUnderHome(t *testing.T) {
	ro := fmt.Sprintf("%d 1 0:%d / %s ro,relatime - ext4 /dev/vdb ro", 20, 20, dockerContainerHome+"/rovol")
	named := fmt.Sprintf("%d 1 0:%d /volumes/data/_data %s rw,relatime - ext4 /dev/vda1 rw", 21, 21, dockerContainerHome+"/named")
	rovolume := fmt.Sprintf("%d 1 0:%d /volumes/cfg/_data %s ro,relatime - ext4 /dev/vda1 rw", 22, 22, dockerContainerHome+"/cfg")
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "id -u") {
			return []byte(dockerNonRootImageUser + "\n"), nil
		}
		if isMountTableExec(args) {
			return []byte(dockerContainerHome + "\n" + dockerWorkspaceDir + "\n" +
				mountinfoLine(1, "/", "overlay") + "\n" + ro + "\n" + named + "\n" + rovolume + "\n"), nil
		}
		return nil, nil
	}))
	p := &dockerProvisioner{containerID: "cid"}
	require.NoError(t, p.prepareNonAccountUser())
	joined := strings.Join(findExecCall(calls, "--user 0:0", "-prune"), " ")
	for _, m := range []string{"/rovol", "/named", "/cfg"} {
		assert.Contains(t, joined, "-path "+shellQuote(dockerContainerHome+m), "mount %s must be pruned", m)
	}
	assert.NotContains(t, joined, "chown -R")
}
