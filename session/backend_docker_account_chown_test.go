package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// accountProbeOwner is the uid:gid the fake account bind mount reports to the
// stat probe of prepareAccountUser, and the uid:gid the chown targets.
const accountProbeOwner = "1000:1000"

// accountMountTableAnswer is the fake container's answer to readMountTable's
// exec for an ACCOUNT session: the resolved account runtime home
// (dockerAccountRuntimeHome, /af-home), the resolved workspace
// (dockerWorkspaceDir, /workspace), and a container mount table made of the
// overlay root mount plus the supplied mount points. It mirrors
// mountTableAnswer (the non-account fixture) but with the account runtime home
// so /af-home-targeted checks name the right path.
func accountMountTableAnswer(mountPoints ...string) []byte {
	lines := []string{dockerAccountRuntimeHome, dockerWorkspaceDir, mountinfoLine(1, "/", "overlay")}
	for i, mp := range mountPoints {
		lines = append(lines, mountinfoLine(10+i, mp, "ext4"))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// isAccountReadMountTableExec reports whether args is the exec call
// prepareAccountUser issues to read the container mount table. Its script
// uses the two-argument readMountTable form (`cd -- "$2" && pwd -P`), which
// isMountTableExec (a substring match for /proc/self/mountinfo) would also
// match — but this check is the one a test asserts when it wants the
// "chown guard read happened" claim with no false positives (the
// /proc/self/mountinfo substring appears in the script whether the call is
// a readMountTable call, a readMountTableFor call, or the bare
// verifyAccountRuntimeBoundary `cat`).
func isAccountReadMountTableExec(args []string) bool {
	if len(args) == 0 || args[0] != "exec" {
		return false
	}
	return strings.Contains(strings.Join(args, " "), `cd -- "$2" && pwd -P`)
}

// accountChownDockerStub answers the account owner stat probe and the new
// readMountTable chown-guard read with mountPoints, recording every docker
// call. Other execs (mkdir+chown, etc.) return (nil, nil), the success answer
// for a stub whose every step says "nothing to report".
func accountChownDockerStub(calls *[][]string, owner string, mountPoints ...string) func(context.Context, []string, ...string) ([]byte, error) {
	return func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		if len(args) == 0 || args[0] != "exec" {
			return nil, nil
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "stat -c %u:%g") {
			return []byte(owner + "\n"), nil
		}
		if isAccountReadMountTableExec(args) {
			return accountMountTableAnswer(mountPoints...), nil
		}
		return nil, nil
	}
}

// newAccountChownProvisioner returns a dockerProvisioner wired for a unit test
// of prepareAccountUser: an account-scoped spec (so the account path dispatch
// takes effect) and a container ID the stub records into each call.
func newAccountChownProvisioner(t *testing.T) *dockerProvisioner {
	t.Helper()
	return &dockerProvisioner{
		spec:        ProvisionSpec{Title: "scoped", Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: t.TempDir()}},
		containerID: "cid",
	}
}

// TestPrepareAccountUser_ChownsHomeAndWorkspaceOnRootFilesystem is the baseline
// for the #5424-style guard applied to the account path: when both /af-home and
// /workspace live on the container's own root filesystem (no repo-supplied bind
// mounts them there), prepareAccountUser runs `mkdir -p` and `chown -h` on both
// as root (0:0), exactly as before the guard — the guard only ADDS the refusing
// branch, it does not regress the safe path.
func TestPrepareAccountUser_ChownsHomeAndWorkspaceOnRootFilesystem(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownDockerStub(&calls, accountProbeOwner)))
	p := newAccountChownProvisioner(t)
	require.NoError(t, p.prepareAccountUser())

	chown := findExecCall(calls, "--user 0:0", "chown", accountProbeOwner)
	require.NotNil(t, chown, "prepareAccountUser must chown /af-home and /workspace as root; calls=%v", calls)
	joined := strings.Join(chown, " ")
	require.Contains(t, joined, "mkdir -p "+shellQuote(dockerAccountRuntimeHome)+" "+shellQuote(dockerWorkspaceDir),
		"prepareAccountUser must create /af-home and /workspace for the chown; call=%v", chown)
	require.Contains(t, joined, "chown -h "+accountProbeOwner+" "+shellQuote(dockerAccountRuntimeHome),
		"prepareAccountUser must chown /af-home on the root fs; call=%v", chown)
	require.Contains(t, joined, "chown -h "+accountProbeOwner+" "+shellQuote(dockerWorkspaceDir),
		"prepareAccountUser must chown /workspace on the root fs; call=%v", chown)
	require.Equal(t, accountProbeOwner, p.containerUser,
		"containerUser records the probed uid:gid so later execs run as it; calls=%v", calls)
}

// TestPrepareAccountUser_ReadsContainerMountTableBeforeChown pins the guard's
// contract: the chown exec never runs without first reading the container's
// own kernel mount table. Before the fix the account path ran mkdir+chown
// unconditionally as the single root exec; after the fix a separate exec reads
// the mount table first, and only a chown-vetting verdict from it lets the
// chown proceed. This is the differential that distinguishes the bug's account
// path from the non-account path that already had it.
func TestPrepareAccountUser_ReadsContainerMountTableBeforeChown(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownDockerStub(&calls, accountProbeOwner)))
	p := newAccountChownProvisioner(t)
	require.NoError(t, p.prepareAccountUser())

	readMount := findExecCall(calls, "/proc/self/mountinfo")
	require.NotNil(t, readMount, "prepareAccountUser must read the container mount table before chowning; calls=%v", calls)
	require.True(t, isAccountReadMountTableExec(readMount),
		"the read must be the two-argument readMountTable call (the chown guard), not the bare cat call verifyAccountRuntimeBoundary uses; call=%v", readMount)

	// The chown exec (if it runs) must come AFTER the mount-table read. The
	// guard cannot vet a chown that has already run, so the ordering is the
	// guard's substance, not a stylistic choice. The readMountTable script
	// contains "mkdir -p" but not "chown", so the substring "chown" uniquely
	// identifies the chown exec (and excludes the readMountTable exec).
	chownIdx := -1
	readIdx := -1
	for i, call := range calls {
		joined := strings.Join(call, " ")
		if isAccountReadMountTableExec(call) && readIdx == -1 {
			readIdx = i
		}
		if call[0] == "exec" && strings.Contains(joined, "chown") && !isAccountReadMountTableExec(call) && chownIdx == -1 {
			chownIdx = i
		}
	}
	require.NotEqual(t, -1, readIdx, "the mount-table read must run; calls=%v", calls)
	require.NotEqual(t, -1, chownIdx, "the chown must run; calls=%v", calls)
	require.Greaterf(t, chownIdx, readIdx,
		"the chown must run AFTER the mount-table read so the guard can vet it; readIdx=%d chownIdx=%d calls=%v",
		readIdx, chownIdx, calls)
}

// TestPrepareAccountUser_WorkspaceInBindMountNotChowned is the central fix: a
// repo-controlled `docker.run_args` bind at /workspace (which the account
// run_args screen accepts — /workspace is not in accountProtectedPath) MUST NOT
// be chowned through. Before the fix the account path did `chown owner
// /workspace` unconditionally, which on a rootful engine re-owns the operator's
// host directory inode through the bind. After the fix the mount-table guard
// detects /workspace's enclosing mount is not "/" and skips the chown, with a
// warning; the HOME chown still runs because /af-home is on the root fs.
func TestPrepareAccountUser_WorkspaceInBindMountNotChowned(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownDockerStub(&calls, accountProbeOwner, dockerWorkspaceDir)))
	p := newAccountChownProvisioner(t)
	require.NoError(t, p.prepareAccountUser())

	chown := findExecCall(calls, "--user 0:0", "mkdir -p "+shellQuote(dockerAccountRuntimeHome))
	require.NotNil(t, chown, "the mkdir step must run for the account home even when /workspace is a mount; calls=%v", calls)
	joined := strings.Join(chown, " ")
	// The chown of /af-home proceeds: it is on the container root fs.
	require.Contains(t, joined, "chown -h "+accountProbeOwner+" "+shellQuote(dockerAccountRuntimeHome),
		"/af-home on the root filesystem is still chowned; call=%v", chown)
	// The chown of /workspace is skipped — its enclosing mount is the bind,
	// not the container root fs, so af would re-own the operator's host dir.
	require.NotContains(t, joined, "chown "+accountProbeOwner+" "+shellQuote(dockerWorkspaceDir),
		"a mount AT /workspace is the operator's directory: it must not be re-owned; call=%v", chown)
	require.NotContains(t, joined, "chown -h "+accountProbeOwner+" "+shellQuote(dockerWorkspaceDir),
		"/workspace in a host bind must not be chowned under any flag; call=%v", chown)
	require.Equal(t, accountProbeOwner, p.containerUser,
		"containerUser records the probed uid:gid so later execs still run as it, even when /workspace is a mount; calls=%v", calls)
}

// TestPrepareAccountUser_WorkspaceBeneathSiblingBindIsStillChowned pins the
// boundary-naming half of the guard: `enclosingMount` matches a mount AT or
// ENCLOSING the path (never a sibling sharing the prefix), so a repo-controlled
// bind at /workspace-data (a sibling of /workspace, not an ancestor) MUST NOT
// make the chown of the real /workspace skip. The path-name boundary
// (`/workspace` vs `/workspace-data`) mirrors the #3398 boundary comment on
// accountProtectedPath: matching the prefix as a substring would over-refuse
// harmless paths, so the guard compares whole-path-components exactly.
func TestPrepareAccountUser_WorkspaceBeneathSiblingBindIsStillChowned(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownDockerStub(&calls, accountProbeOwner, dockerWorkspaceDir+"-data")))
	p := newAccountChownProvisioner(t)
	require.NoError(t, p.prepareAccountUser())

	chown := findExecCall(calls, "--user 0:0", "mkdir -p "+shellQuote(dockerAccountRuntimeHome))
	require.NotNil(t, chown, "both /af-home and /workspace must be chowned; calls=%v", calls)
	joined := strings.Join(chown, " ")
	require.Contains(t, joined, "chown -h "+accountProbeOwner+" "+shellQuote(dockerWorkspaceDir),
		"a sibling-named mount /workspace-data must not affect /workspace; call=%v", chown)
}

// TestPrepareAccountUser_HomeInBindMountNotChowned checks the symmetric half:
// if /af-home itself resolves to a mount (an image-declared VOLUME, a
// --volumes-from donor an operator accepted inline, af's own account bind at
// /af-account does not collide but a tmpfs at /af-home would), its chown is
// skipped too — the guard treats /af-home and /workspace by the same rule.
func TestPrepareAccountUser_HomeInBindMountNotChowned(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownDockerStub(&calls, accountProbeOwner, dockerAccountRuntimeHome)))
	p := newAccountChownProvisioner(t)
	require.NoError(t, p.prepareAccountUser())

	chown := findExecCall(calls, "--user 0:0", "mkdir -p "+shellQuote(dockerAccountRuntimeHome))
	require.NotNil(t, chown, "the mkdir step must run for /workspace even when /af-home is a mount; calls=%v", calls)
	joined := strings.Join(chown, " ")
	require.NotContains(t, joined, "chown "+accountProbeOwner+" "+shellQuote(dockerAccountRuntimeHome),
		"a mount AT /af-home is the operator's directory: it must not be re-owned; call=%v", chown)
	require.NotContains(t, joined, "chown -h "+accountProbeOwner+" "+shellQuote(dockerAccountRuntimeHome),
		"/af-home in a host bind must not be chowned under any flag; call=%v", chown)
	require.Contains(t, joined, "chown -h "+accountProbeOwner+" "+shellQuote(dockerWorkspaceDir),
		"/workspace on the root filesystem is still chowned; call=%v", chown)
}

// TestPrepareAccountUser_BothInBindMountsChownsNothing is the most adversarial
// path: a repo controls run_args to bind /workspace AND the image declares a
// VOLUME at /af-home. Both chowns are skipped, only `mkdir -p` runs (a no-op on
// the binds the kernel already created), the operator's host files at each
// target are untouched, and containerUser stays empty — a refused provisioning
// must not switch the exec user.
func TestPrepareAccountUser_BothInBindMountsChownsNothing(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownDockerStub(&calls, accountProbeOwner,
		dockerAccountRuntimeHome, dockerWorkspaceDir)))
	p := newAccountChownProvisioner(t)
	require.NoError(t, p.prepareAccountUser())

	chown := findExecCall(calls, "--user 0:0", "mkdir -p "+shellQuote(dockerAccountRuntimeHome))
	require.NotNil(t, chown, "the mkdir step must run; calls=%v", calls)
	joined := strings.Join(chown, " ")
	require.NotContains(t, joined, "chown",
		"nothing is chowned when /af-home and /workspace are both mounts; call=%v", chown)
	require.Equal(t, accountProbeOwner, p.containerUser,
		"containerUser records the probed uid:gid even when the chowns were skipped, so later execs run as it (mirrors the non-account path); calls=%v", calls)
}

// TestPrepareAccountUser_ChownUsesFlagH mirrors the non-account path's -h
// contract: a symlinked /af-home or /workspace (resolved by readMountTable's
// `pwd -P`, so realHome/realWorkspace is the symlink-free target) is unlikely,
// but the chown the guard emits must still use `-h` so a symlink at the target
// path is re-owned itself rather than its referent — `chown X <path>`
// dereferences a command-line symlink by default on coreutils, and a future
// image shipping a symlink at /af-home would otherwise have the chown land
// outside the path. busybox and coreutils both implement `-h`.
func TestPrepareAccountUser_ChownUsesFlagH(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownDockerStub(&calls, accountProbeOwner)))
	p := newAccountChownProvisioner(t)
	require.NoError(t, p.prepareAccountUser())

	chown := findExecCall(calls, "--user 0:0", "chown", accountProbeOwner)
	require.NotNil(t, chown, "the chown must run on the root fs; calls=%v", calls)
	joined := strings.Join(chown, " ")
	for _, path := range []string{dockerAccountRuntimeHome, dockerWorkspaceDir} {
		assert.Contains(t, joined, "chown -h "+accountProbeOwner+" "+shellQuote(path),
			"each chown must use -h so a symlink at the target is re-owned itself, not its referent; call=%v", chown)
	}
	assert.NotContains(t, joined, "chown "+accountProbeOwner+" ",
		"no -h-less chown may be emitted (a -h-less chown dereferences command-line symlinks); call=%v", chown)
}

// TestPrepareAccountUser_UnreadableMountTableRefusesTheChown pins the fail-
// closed posture: an unreadable or ununderstood container mount table is an
// error, never a silent empty prune set. The non-account path already refuses
// here (#5424 review); the account path's missing guard meant the chown ran
// unconditionally, so a "the mount table is unreadable" outcome re-owned host
// files in the cases the guard was meant to refuse. With the guard applied,
// the account path refuses and never chowns, mirroring the non-account posture.
func TestPrepareAccountUser_UnreadableMountTableRefusesTheChown(t *testing.T) {
	for name, answer := range map[string]func() ([]byte, error){
		"exec fails":   func() ([]byte, error) { return []byte("sh: cat: not found"), errors.New("exit status 127") },
		"empty answer": func() ([]byte, error) { return nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				if len(args) > 0 && args[0] == "exec" {
					joined := strings.Join(args, " ")
					if strings.Contains(joined, "stat -c %u:%g") {
						return []byte(accountProbeOwner + "\n"), nil
					}
					if isAccountReadMountTableExec(args) {
						return answer()
					}
				}
				return nil, nil
			}))
			p := newAccountChownProvisioner(t)
			err := p.prepareAccountUser()
			require.Error(t, err, "an unreadable mount table must refuse the chown, not proceed without it")
			assert.Contains(t, err.Error(), "mount table",
				"the refusal must name the mount table so an operator sees the real cause")
			assert.Nil(t, findExecCall(calls, "chown "+accountProbeOwner),
				"no chown may run without a usable mount table; calls=%v", calls)
			assert.Nil(t, findExecCall(calls, "chown -h "+accountProbeOwner),
				"no -h-form chown may run without a usable mount table either; calls=%v", calls)
			assert.Empty(t, p.containerUser,
				"the user must not be recorded when the chown was refused; calls=%v", calls)
		})
	}
}

// TestPrepareAccountUser_UnparseableMountTableRefusesTheChown pins the parse-
// failure half of the fail-closed posture: an answer that returns SOMETHING but
// not a valid mount table (garbage the readMountTable shape accepts, or a
// mountinfo with no "/" mount) is an error to parseMountPoints, and the account
// chown is refused — never guessed — mirroring the non-account path.
func TestPrepareAccountUser_UnparseableMountTableRefusesTheChown(t *testing.T) {
	for name, mt := range map[string]string{
		"garbage mountinfo": "/af-home\n/workspace\nnot a mount table\n",
		"no root mount":     "/af-home\n/workspace\n" + mountinfoLine(2, "/proc", "proc") + "\n",
		"relative mount":    "/af-home\n/workspace\n9 1 0:9 / rel rw - ext4 s rw\n",
		"short line":        "/af-home\n/workspace\n" + mountinfoLine(1, "/", "overlay") + "\n36 35 98:0\n",
	} {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				if len(args) > 0 && args[0] == "exec" {
					joined := strings.Join(args, " ")
					if strings.Contains(joined, "stat -c %u:%g") {
						return []byte(accountProbeOwner + "\n"), nil
					}
					if isAccountReadMountTableExec(args) {
						return []byte(mt), nil
					}
				}
				return nil, nil
			}))
			p := newAccountChownProvisioner(t)
			err := p.prepareAccountUser()
			require.Error(t, err, "an unparseable mount table must refuse the chown")
			assert.Contains(t, err.Error(), "mount table",
				"the refusal must name the mount table")
			assert.Nil(t, findExecCall(calls, "chown "+accountProbeOwner),
				"no chown may run over an unparseable mount table; calls=%v", calls)
			assert.Nil(t, findExecCall(calls, "chown -h "+accountProbeOwner),
				"no -h-form chown may run over an unparseable mount table either; calls=%v", calls)
			assert.Empty(t, p.containerUser,
				"the user must not be recorded when the chown was refused; calls=%v", calls)
		})
	}
}

// TestPrepareAccountUser_MountTableFailureCarriesOpClass pins the operation
// class of the mount-table failure paths: parseMountPoints' verdict on an
// unparseable answer names af's own inability, not a docker write — local,
// never dockerOpRootExec a full data root could attach an ENOSPC verdict to.
// The exec-failing path that readMountTable wraps before parse is a root-exec
// class — the read itself runs as 0:0. Both match the non-account path's
// posture (its #5424 review framed the same split).
func TestPrepareAccountUser_MountTableFailureCarriesOpClass(t *testing.T) {
	t.Run("unparseable answer is local", func(t *testing.T) {
		var calls [][]string
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			if len(args) > 0 && args[0] == "exec" {
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "stat -c %u:%g") {
					return []byte(accountProbeOwner + "\n"), nil
				}
				if isAccountReadMountTableExec(args) {
					return []byte("/af-home\n/workspace\nnot a mount table\n"), nil
				}
			}
			return nil, nil
		}))
		p := newAccountChownProvisioner(t)
		err := p.prepareAccountUser()
		require.Error(t, err)
		var se *dockerStepError
		require.True(t, errors.As(err, &se), "the mount-table parse failure must carry an operation class")
		assert.Equal(t, dockerOpLocal, se.op,
			"unparseable mount table output is af's failure, never a filesystem verdict")
	})
	t.Run("exec failure is root-exec class", func(t *testing.T) {
		var calls [][]string
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			if len(args) > 0 && args[0] == "exec" {
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "stat -c %u:%g") {
					return []byte(accountProbeOwner + "\n"), nil
				}
				if isAccountReadMountTableExec(args) {
					return []byte("sh: cat: not found"), errors.New("exit status 127")
				}
			}
			return nil, nil
		}))
		p := newAccountChownProvisioner(t)
		err := p.prepareAccountUser()
		require.Error(t, err)
		var se *dockerStepError
		require.True(t, errors.As(err, &se), "the exec-failure class must be set")
		assert.Equal(t, dockerOpRootExec, se.op,
			"the mount-table read runs as root (0:0), so a failed exec is the root-exec class")
	})
}

// TestPrepareAccountUser_Differential_SkipsWorkspaceChownLikeNonAccount is the
// differential the bug report reproduces: under an identical /workspace
// bind-mount scenario, a non-account session skips the chown and warns via its
// #5424 guard; an account session ran the chown unconditionally and re-owned
// the host directory's inode. After the fix both paths apply the same mount-
// table contract, so both skip the chown of /workspace under the bind.
func TestPrepareAccountUser_Differential_SkipsWorkspaceChownLikeNonAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec ProvisionSpec
	}{
		{name: "account", spec: ProvisionSpec{Title: "scoped",
			Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: "/af-account"}}},
		{name: "non-account", spec: ProvisionSpec{Title: "non-account"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			stub := func(_ context.Context, _ []string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				if len(args) == 0 || args[0] != "exec" {
					return nil, nil
				}
				joined := strings.Join(args, " ")
				switch {
				case strings.Contains(joined, "stat -c %u:%g"):
					return []byte(accountProbeOwner + "\n"), nil // account path
				case strings.Contains(joined, "id -u"):
					return []byte(accountProbeOwner + "\n"), nil // non-account path
				case isAccountReadMountTableExec(args):
					return accountMountTableAnswer(dockerWorkspaceDir), nil // account: /workspace is a mount
				case isMountTableExec(args):
					// Non-account's readMountTable is matched here too, since
					// isAccountReadMountTableExec is stricter. The non-account
					// mountinfo should also report a /workspace mount so the
					// guard skips its chown.
					return mountTableAnswer(dockerWorkspaceDir), nil
				}
				return nil, nil
			}
			t.Cleanup(SetDockerExecForTest(stub))
			p := &dockerProvisioner{spec: tc.spec, containerID: "cid"}
			require.NoError(t, p.prepareContainerUser())

			// Both paths must skip the chown of /workspace; neither re-owns
			// the operator's host directory through the bind.
			for _, call := range calls {
				if len(call) == 0 || call[0] != "exec" {
					continue
				}
				joined := strings.Join(call, " ")
				if !strings.Contains(joined, "--user 0:0") {
					continue
				}
				assert.NotContainsf(t, joined, "chown "+accountProbeOwner+" "+shellQuote(dockerWorkspaceDir),
					"%s path: the /workspace bind must NOT be chowned under any form; call=%v", tc.name, call)
				assert.NotContainsf(t, joined, "chown -h "+accountProbeOwner+" "+shellQuote(dockerWorkspaceDir),
					"%s path: the /workspace bind must NOT be chowned under any flag; call=%v", tc.name, call)
			}
		})
	}
}

// TestPrepareAccountUser_NoOpForNonAccount pins the dispatch half: when
// spec.Account.Dir is empty, prepareAccountUser returns early (no chown, no
// mount-table read), exactly as before the guard. The non-account path owns
// that scenario, and a stray probe or chown from the account path would race
// the non-account path's containerUser assignment.
func TestPrepareAccountUser_NoOpForNonAccount(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	}))
	p := &dockerProvisioner{
		spec:        ProvisionSpec{Title: "non-account"},
		containerID: "cid",
	}
	require.NoError(t, p.prepareAccountUser(),
		"prepareAccountUser must be a no-op for a non-account session")
	require.Empty(t, calls, "an account-scoped session's prepareAccountUser must not call docker when Account.Dir is empty; calls=%v", calls)
	require.Equal(t, "", p.containerUser, "the account probe must not set containerUser on a non-account session")
}

// TestPrepareAccountUser_SkippedChownLogsWarning mirrors procedure #15 of the
// test plan: when the mount-table guard skips a chown, prepareAccountUser MUST
// log a warning via log.WarningLog naming the path, its enclosing mount, and
// the owner it would have re-owned — matching prepareNonAccountUser's #5424
// wording ("docker session workspace %s is inside the mount %s ... not
// chowning it to %s — make sure it is writable by that user"). The warning is
// the operator's only signal that the skipped path may not be writable by
// the coming clone; a missing warning would let the clone fail opaquely.
func TestPrepareAccountUser_SkippedChownLogsWarning(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mountPath string
		wantSubs  []string
	}{
		{
			name:      "workspace in a bind",
			mountPath: dockerWorkspaceDir,
			wantSubs:  []string{"docker session workspace", dockerWorkspaceDir, "not chowning it", accountProbeOwner, "writable by that user"},
		},
		{
			name:      "home in a bind",
			mountPath: dockerAccountRuntimeHome,
			wantSubs:  []string{"docker session HOME", dockerAccountRuntimeHome, "not chowning it", accountProbeOwner, "writable by that user"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var warnings bytes.Buffer
			log.CaptureOutput(t, log.WarningLog, "", &warnings)
			var calls [][]string
			t.Cleanup(SetDockerExecForTest(accountChownDockerStub(&calls, accountProbeOwner, tc.mountPath)))
			p := newAccountChownProvisioner(t)
			require.NoError(t, p.prepareAccountUser())

			got := warnings.String()
			for _, want := range tc.wantSubs {
				assert.Containsf(t, got, want,
					"the skip warning must name %q so an operator sees what was skipped and why; warnings=%q", want, got)
			}
			// The enclosing mount's path must appear too (it's the
			// distinction between "skipped" and "skipped because of X").
			assert.Contains(t, got, "is inside the mount",
				"the skip warning must name the enclosing mount the path is inside; warnings=%q", got)
		})
	}
}

// TestPrepareAccountUser_SkippedChown_NoWarningForKeptPath is the negative half
// of the logging contract: when both /af-home and /workspace are on the root
// filesystem and are chowned, NO skip warning is logged. A false-positive
// warning would train an operator to ignore the very signal they need when a
// /workspace bind appears.
func TestPrepareAccountUser_SkippedChown_NoWarningForKeptPath(t *testing.T) {
	var warnings bytes.Buffer
	log.CaptureOutput(t, log.WarningLog, "", &warnings)
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownDockerStub(&calls, accountProbeOwner)))
	p := newAccountChownProvisioner(t)
	require.NoError(t, p.prepareAccountUser())

	assert.NotContains(t, warnings.String(), "not chowning it",
		"no skip warning must appear when both /af-home and /workspace are chowned on the root fs; warnings=%q", warnings.String())
}
