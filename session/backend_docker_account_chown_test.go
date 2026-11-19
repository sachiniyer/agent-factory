package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
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
	joined := strings.Join(args, " ")
	// verifyWriteTargets' read shares the `cd -- "$2" && pwd -P` clause; the
	// leafSymlinkMarker in its script is what tells the two readers apart.
	return strings.Contains(joined, `cd -- "$2" && pwd -P`) && !strings.Contains(joined, leafSymlinkMarker)
}

// accountChownScenario parameterizes the container answers the chown-guard
// tests drive: the mount table's extra mount points, and the `test -w &&
// test -x` writability probe's verdict for a mount af declines to re-own.
// The guard asks no mount-type question — mountinfo cannot say what backs a
// mount and af never chowns through one — so the scenario has no answer for
// it.
//
// probeErr, when non-nil, is the error the writability probe returns verbatim
// — used to drive an INFRASTRUCTURE-failure shape (docker CLI exit
// 125/126/127, a transport error) that the reportSkippedMount probe must
// classify dockerOpDaemonWrite rather than dockerOpLocal, so provision() can
// join dockerRootFSFullEvidence. When probeErr is nil, the writable field
// drives the verdict shape: true returns success (the mounted dir is
// writable), false returns a genuine *exec.ExitError with ExitCode 1 (`test`'s
// false verdict) matching the production error TYPE p.docker returns from a
// non-zero docker exec exit.
//
// probeOut, when probeErr is non-nil, is the combined-output byte slice
// returned alongside the error. Production's cmd.CombinedOutput captures the
// docker CLI's merged stdout/stderr into `out` and leaves the returned
// *exec.ExitError's Stderr nil; reportSkippedMount attaches `out` to the
// refusal via the "(%s)" clause. Mirror the contract so the daemon's errno
// text surfaces as prose even though the typed errno travels only by way of
// the op class (the *exec.ExitError's Stderr is intentionally NOT set so the
// stub stays faithful to production, where CombinedOutput leaves it nil).
type accountChownScenario struct {
	mountPoints []string
	writable    bool
	probeErr    error
	probeOut    []byte
}

// dockerExecExitErr returns a genuine *exec.ExitError whose ExitCode is code
// — the production error TYPE p.docker returns from a non-zero docker CLI
// exit. The Stderr field is intentionally left to whatever the sh -c "exit
// N" Run produces (empty for a bare exit), mirroring production, where
// cmd.CombinedOutput ships the merged stdout/stderr as the returned byte
// slice and leaves *exec.ExitError.Stderr nil — the test case's prose is
// delivered alongside via accountChownScenario.probeOut, not via Stderr. The
// chown guard's `test -w` stub used to model only the error's %v TEXT
// (errors.New("exit status 1")); once reportSkippedMount branches on the
// error TYPE (skippedMountProbeClass), the stub must model the type too,
// mirroring baseTmuxAbsentExitErr in tab_persist_test.go.
func dockerExecExitErr(code int) *exec.ExitError {
	err := exec.Command(shForSessionTests, "-c", fmt.Sprintf("exit %d", code)).Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr
	}
	// Should not happen on a Posix test host; keep a syntactic fallback so a
	// missing sh degrades to a stub the classifier still sees as a real
	// *exec.ExitError with the requested code rather than panicking on a nil
	// ProcessState.
	return &exec.ExitError{}
}

// chownProbeTestVerdictErr is the genuine *exec.ExitError a `test -w &&
// test -x` probe returns when the directory is not writable by the session's
// user — the production shape skippedMountProbeClass matches as the verdict
// (ExitCode 1) and classifies dockerOpLocal. Use it instead of a plain
// errors.New("exit status 1") so the stub faithfully models the production
// error TYPE, not only its text.
func chownProbeTestVerdictErr() error {
	return dockerExecExitErr(1)
}

// accountChownScenarioStub answers the account owner stat probe, the
// readMountTable chown-guard read, and the writability probe, recording every
// docker call. Other execs (mkdir+chown, etc.) return (nil, nil), the
// success answer for a stub whose every step says "nothing to report".
func accountChownScenarioStub(calls *[][]string, owner string, scenario accountChownScenario) func(context.Context, []string, ...string) ([]byte, error) {
	return func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		if len(args) == 0 {
			return nil, nil
		}
		joined := strings.Join(args, " ")
		switch {
		case args[0] != "exec":
			return nil, nil
		case strings.Contains(joined, "stat -c %u:%g"):
			return []byte(owner + "\n"), nil
		case isAccountReadMountTableExec(args):
			return accountMountTableAnswer(scenario.mountPoints...), nil
		case strings.Contains(joined, "test -w"):
			// The writability probe af runs as the session's user when a
			// chown target sits inside a mount it must not re-own. A
			// probeErr models a non-verdict failure shape (docker CLI
			// exit 125/126/127, a transport error) the guard must classify
			// as docker/exec infrastructure, not the `test` verdict. probeOut
			// ships the daemon-supplied stdout/stderr CombinedOutput would
			// have returned alongside — `test` writes nothing container-side
			// and the docker CLI's combined output carries the daemon's
			// diagnostic, which reportSkippedMount attaches to the refusal
			// via the "(%s)" clause.
			if scenario.probeErr != nil {
				return scenario.probeOut, scenario.probeErr
			}
			if scenario.writable {
				return nil, nil
			}
			// The verdict shape is a real *exec.ExitError with ExitCode 1
			// — the production TYPE the classifier branches on, not the
			// bare %v text a plain errors.New("exit status 1") models.
			return nil, chownProbeTestVerdictErr()
		default:
			return nil, nil
		}
	}
}

// accountChownDockerStub answers the account owner stat probe and the new
// readMountTable chown-guard read with mountPoints, plus a writable verdict
// on the session-user probe, so a skipped target proceeds with the guard's
// warning exactly as the pre-R6 contract had it. Other execs (mkdir+chown,
// etc.) return (nil, nil), the success answer for a stub whose every step
// says "nothing to report".
func accountChownDockerStub(calls *[][]string, owner string, mountPoints ...string) func(context.Context, []string, ...string) ([]byte, error) {
	return accountChownScenarioStub(calls, owner, accountChownScenario{
		mountPoints: mountPoints,
		writable:    true,
	})
}

// execShellScript returns the script a `docker exec … sh -c <script>` call
// runs, or "" when the call is not a sh -c exec.
func execShellScript(call []string) string {
	for i, arg := range call {
		if arg == "-c" && i+1 < len(call) {
			return call[i+1]
		}
	}
	return ""
}

// rootExecScripts returns the sh -c scripts of every root (0:0) docker exec in
// calls — the whole set of commands af ran as container root during
// preparation.
func rootExecScripts(calls [][]string) []string {
	var scripts []string
	for _, call := range calls {
		if len(call) == 0 || call[0] != "exec" || !strings.Contains(strings.Join(call, " "), "--user 0:0") {
			continue
		}
		scripts = append(scripts, execShellScript(call))
	}
	return scripts
}

// pathWithinTarget reports whether root is the target path itself or one of
// its ancestors — the direction #7571 review R10 requires: a recursive chown
// or a find traversal rooted at an ANCESTOR reaches the target, so the
// oracle must flag operands like `chown -R <o> '/'` and start points like
// `find '/workspace' …`, not only the target spelled verbatim.
func pathWithinTarget(target, root string) bool {
	target, root = path.Clean(target), path.Clean(root)
	if root == "/" {
		return strings.HasPrefix(target, "/")
	}
	return target == root || strings.HasPrefix(target, root+"/")
}

// commandBasename returns the program a command word names, quote-stripped
// and reduced to its basename — /bin/chown and /usr/bin/chown are chown as
// surely as the bare name (#7571 review R10).
func commandBasename(word string) string {
	return path.Base(strings.Trim(word, `'"`))
}

// unFindPattern reverses findLiteralPattern's metacharacter escapes so a
// -path prune entry compares literally with the target.
func unFindPattern(p string) string {
	return strings.NewReplacer(`\\`, `\`, `\*`, `*`, `\?`, `?`, `\[`, `[`, `\]`, `]`).Replace(p)
}

// chownFieldsHit reports whether the fields following a chown program name
// take target as an operand — or an ancestor of it when the command is
// recursive (`-R`, bundled `-…R…`, or `--recursive`), since only then does
// an ancestor operand reach the target (#7571 review R10). A non-recursive
// `chown -h <o> '/af-home'` does NOT chown /af-home/workspace, and the
// oracle says so. Operand collection ends at find's -exec terminator (`+`
// or the `\` remnant of `\;`).
func chownFieldsHit(fields []string, target string) bool {
	end := len(fields)
	for i, f := range fields {
		if f == "+" || f == `\` || f == `\;` {
			end = i
			break
		}
	}
	fields = fields[:end]
	recursive := false
	for _, f := range fields {
		if f == "--recursive" || (strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.Contains(f, "R")) {
			recursive = true
		}
	}
	for _, f := range fields {
		if strings.HasPrefix(f, "-") {
			continue
		}
		operand := path.Clean(strings.Trim(f, `'"`))
		if operand == target || (recursive && pathWithinTarget(target, operand)) {
			return true
		}
	}
	return false
}

// findStartPoints returns the path operands of a find command — the fields
// after `find` up to the first expression token (an option, `!`, `,` or a
// parenthesis, possibly backslash-escaped). A target inside any of them is
// a chown victim when the command runs -exec chown.
func findStartPoints(fields []string) []string {
	var starts []string
	for _, f := range fields {
		if c := f[0]; c == '-' || c == '(' || c == ')' || c == '!' || c == ',' || c == '\\' {
			break
		}
		starts = append(starts, strings.Trim(f, `'"`))
	}
	return starts
}

// findPrunePaths returns the unescaped -path arguments of a find command —
// the prune entries. A target at or beneath one is cut out of the traversal
// before -exec ever sees it, so it is not chowned.
func findPrunePaths(fields []string) []string {
	var prunes []string
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "-path" {
			prunes = append(prunes, unFindPattern(strings.Trim(fields[i+1], `'"`)))
			i++
		}
	}
	return prunes
}

// findChownHits reports whether a find command chowns the target: it must
// carry `-exec chown`, and then either a literal -exec operand names the
// target (same operand rule as a direct chown) or a start point is the
// target-or-ancestor while no -path prune entry covers it.
func findChownHits(fields []string, target string) bool {
	execIdx := -1
	for i, f := range fields {
		if f == "-exec" && i+1 < len(fields) && commandBasename(fields[i+1]) == "chown" {
			execIdx = i
			break
		}
	}
	if execIdx < 0 {
		return false
	}
	if chownFieldsHit(fields[execIdx+2:], target) {
		return true
	}
	for _, p := range findPrunePaths(fields) {
		if pathWithinTarget(target, p) {
			return false
		}
	}
	for _, s := range findStartPoints(fields) {
		if pathWithinTarget(target, s) {
			return true
		}
	}
	return false
}

// rootScriptChowns reports whether a shell snippet runs a chown that reaches
// target: ANY chown program matched by basename (bare `chown`, `/bin/chown`,
// …), and either an operand equal to the target — in ANY position, so the
// unguarded mutant `chown -h <owner> '/af-home' '/workspace'` is caught even
// though /workspace follows another operand rather than the owner (#7563
// R5) — or a RECURSIVE chown operand that is one of the target's ancestors
// (`chown -R <o> '/'`), or a `find … -exec chown` whose start point is the
// target-or-ancestor unless a -path prune entry covers it (#7571 review
// R10). The snippet is split into commands on the &&, ||, ; and | boundaries
// af's scripts join on; shell quote wrapping is stripped before comparing,
// so a quoted and a bare operand spell the same path. A target named only as
// find's -path argument (a prune entry) is NOT a chown operand and does not
// match.
func rootScriptChowns(script, target string) bool {
	commands := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n").Replace(script)
	for _, command := range strings.Split(commands, "\n") {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			continue
		}
		switch commandBasename(fields[0]) {
		case "chown":
			if chownFieldsHit(fields[1:], target) {
				return true
			}
		case "find":
			if findChownHits(fields[1:], target) {
				return true
			}
		}
	}
	return false
}

// requireNoRootChownOn asserts no root exec in calls hands target to any
// chown command — the operand-level assertion #7563 R5 requires, so a
// regression that reinstates an unguarded chown goes red however the
// operands are ordered (`chown -h <o> '/af-home' '/workspace'` included).
func requireNoRootChownOn(t *testing.T, calls [][]string, target string) {
	t.Helper()
	for _, script := range rootExecScripts(calls) {
		require.Falsef(t, rootScriptChowns(script, target),
			"the path must not be an operand of any root chown; target=%s script=%v", target, script)
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
	// The check is operand-level, not shape-matched: /workspace must not be
	// ANY operand of ANY root chown, so the unguarded mutant
	// `chown -h <o> '/af-home' '/workspace'` — where /workspace follows
	// another operand rather than the owner — goes red here too (#7563 R5).
	requireNoRootChownOn(t, calls, dockerWorkspaceDir)
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
	// Operand-level, not shape-matched: /af-home must not be ANY operand of
	// ANY root chown, so a mutant `chown -h <o> '/workspace' '/af-home'`
	// goes red here as well (#7563 R5).
	requireNoRootChownOn(t, calls, dockerAccountRuntimeHome)
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
	requireNoRootChownOn(t, calls, dockerAccountRuntimeHome)
	requireNoRootChownOn(t, calls, dockerWorkspaceDir)
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
			requireNoRootChownOn(t, calls, dockerAccountRuntimeHome)
			requireNoRootChownOn(t, calls, dockerWorkspaceDir)
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
			requireNoRootChownOn(t, calls, dockerAccountRuntimeHome)
			requireNoRootChownOn(t, calls, dockerWorkspaceDir)
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
					// Non-account's reads land here too, since
					// isAccountReadMountTableExec is stricter. The non-account
					// mountinfo should also report a /workspace mount so the
					// guard skips its chown.
					return probeAnswer(args, dockerWorkspaceDir), nil
				}
				return nil, nil
			}
			t.Cleanup(SetDockerExecForTest(stub))
			p := &dockerProvisioner{spec: tc.spec, containerID: "cid"}
			require.NoError(t, p.prepareContainerUser())

			// Both paths must skip the chown of /workspace; neither re-owns
			// the operator's host directory through the bind. Operand-level:
			// /workspace must not be ANY operand of ANY root chown on either
			// path, so the unguarded mutant `chown -h <o> '/af-home'
			// '/workspace'` — or its non-account twin — goes red (#7563 R5).
			for _, script := range rootExecScripts(calls) {
				assert.Falsef(t, rootScriptChowns(script, dockerWorkspaceDir),
					"%s path: the /workspace bind must NOT be chowned under any form; script=%v", tc.name, script)
			}
		})
	}
}

// TestPrepareAccountUser_NoMountClassificationHappens pins the R6 direction
// change: af NEVER asks `docker inspect` what backs a mount — a
// local-driver volume can be bind-backed via volume-opt=o=bind, so the only
// mount af writes through is the container's own root filesystem. A mount at
// /workspace is skipped (warn + proceed when the session's user can already
// write it) without any inspect call at all.
func TestPrepareAccountUser_NoMountClassificationHappens(t *testing.T) {
	var warnings bytes.Buffer
	log.CaptureOutput(t, log.WarningLog, "", &warnings)
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownScenarioStub(&calls, accountProbeOwner, accountChownScenario{
		mountPoints: []string{dockerWorkspaceDir},
		writable:    true,
	})))
	p := newAccountChownProvisioner(t)
	require.NoError(t, p.prepareAccountUser())
	for _, call := range calls {
		assert.NotEqual(t, "inspect", call[0],
			"the chown guard must not consult `docker inspect` — there is no mount-type carve-out; call=%v", call)
	}
	requireNoRootChownOn(t, calls, dockerWorkspaceDir)
	assert.Contains(t, warnings.String(), "not chowning it",
		"a writable mount still warns that its chown was skipped; warnings=%q", warnings.String())
	require.Equal(t, accountProbeOwner, p.containerUser)
}

// TestPrepareAccountUser_UnwritableMountRefusesNamingTheMount is the R6
// error contract: when af declines to prepare /workspace because a mount
// sits there AND the session's user cannot write it, the session's ERROR
// names the mount and the remedy — so the operator is not left with a bare
// clone "Permission denied" to debug. This holds for every mount kind:
// bind, named volume, image VOLUME or tmpfs are indistinguishable in
// mountinfo and none may be chowned.
func TestPrepareAccountUser_UnwritableMountRefusesNamingTheMount(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownScenarioStub(&calls, accountProbeOwner, accountChownScenario{
		mountPoints: []string{dockerWorkspaceDir},
		writable:    false,
	})))
	p := newAccountChownProvisioner(t)
	err := p.prepareAccountUser()
	require.Error(t, err, "an unwritable mount at /workspace must refuse at prepare, not fail later at the clone")
	for _, want := range []string{
		dockerWorkspaceDir, // the affected mount
		accountProbeOwner,  // who cannot write it
		"chown",            // the suggested remedy's mechanism
		"writable",         // what the operator must arrange
		"docker.run_args",  // where the mount came from
	} {
		assert.Containsf(t, err.Error(), want, "the refusal must name %q; err=%v", want, err)
	}
	var se *dockerStepError
	require.True(t, errors.As(err, &se), "the refusal must carry an operation class")
	assert.Equal(t, dockerOpLocal, se.op, "the refusal is af's own decision, never a filesystem verdict")
	requireNoRootChownOn(t, calls, dockerWorkspaceDir)
	assert.Empty(t, p.containerUser, "a refused provisioning must not switch the exec user")
}

// TestPrepareAccountUser_SkippedMountProbeClassSplitsByExitCode pins the
// classification reportSkippedMount makes on its `test -w && test -x` probe
// failure: the genuine `test` false verdict (ExitCode 1) is af's own
// decision — dockerOpLocal, the only class provision() excludes from
// dockerRootFSFullEvidence so a local refusal does not borrow a capacity
// verdict on a coincidentally-full root — while any other shape (docker CLI
// exit 125/126/127, a transport error) is docker/exec infrastructure that
// wrote nothing container-side, so it carries dockerOpDaemonWrite and
// provision() may join the typed-errno evidence a full data root hit loses
// into prose. #7571 added reportSkippedMount pinning only exit status 1, so
// an infrastructure failure (the bug: a provably read-only exec whose
// dockerd can't bookkeep it on a full data root) inherited the verdict's
// dockerOpLocal and lost the evidence path. The sibling read-only exec
// probes (prepareNonAccountUser's id -u and prepareAccountUser's stat -c
// %u:%g) already classify the same infra shape dockerOpDaemonWrite; this
// pins that the skipped-mount probe does too. The verdict case mirrors
// TestPrepareAccountUser_UnwritableMountRefusesNamingTheMount but uses a
// real *exec.ExitError{ExitCode 1} straight, so the differential is visible
// here and not only by reading the sibling test.
func TestPrepareAccountUser_SkippedMountProbeClassSplitsByExitCode(t *testing.T) {
	for name, tc := range map[string]struct {
		probeErr  error
		probeOut  []byte
		wantOp    dockerOpClass
		wantProse string
	}{
		"exit 1 is the test verdict — local, evidence suppressed": {
			probeErr:  chownProbeTestVerdictErr(),
			wantOp:    dockerOpLocal,
			wantProse: "exit status 1",
		},
		"exit 125 with daemon ENOSPC stderr is infrastructure — daemon-write": {
			probeErr:  dockerExecExitErr(125),
			probeOut:  []byte("docker: Error response from daemon: mkdir /var/lib/docker/containers/abc/execs.def: no space left on device"),
			wantOp:    dockerOpDaemonWrite,
			wantProse: "no space left on device",
		},
		"exit 126 (command not executable) is infrastructure — daemon-write": {
			probeErr:  dockerExecExitErr(126),
			probeOut:  []byte("exec: \"test\": permission denied"),
			wantOp:    dockerOpDaemonWrite,
			wantProse: "permission denied",
		},
		"exit 127 (command not found) is infrastructure — daemon-write": {
			probeErr:  dockerExecExitErr(127),
			probeOut:  []byte("sh: 1: test: not found"),
			wantOp:    dockerOpDaemonWrite,
			wantProse: "not found",
		},
		"transport error (not an *exec.ExitError) is infrastructure — daemon-write": {
			probeErr:  errors.New("dial unix /var/run/docker.sock: connect: no such file or directory"),
			wantOp:    dockerOpDaemonWrite,
			wantProse: "no such file or directory",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			t.Cleanup(SetDockerExecForTest(accountChownScenarioStub(&calls, accountProbeOwner, accountChownScenario{
				mountPoints: []string{dockerWorkspaceDir},
				probeErr:    tc.probeErr,
				probeOut:    tc.probeOut,
			})))
			p := newAccountChownProvisioner(t)
			err := p.prepareAccountUser()
			require.Error(t, err, "a failed probe must still refuse, not proceed on unknown writability")
			var se *dockerStepError
			require.True(t, errors.As(err, &se), "the refusal must carry an operation class; err=%v", err)
			assert.Equalf(t, tc.wantOp, se.op,
				"the probe class must split verdict (%v) from infrastructure (%v); err=%v",
				dockerOpLocal, dockerOpDaemonWrite, err)
			// The classification changes the evidence path at provision(),
			// not the message the operator reads — the refusal still names
			// the mount and the remedy under any op class.
			for _, want := range []string{
				dockerWorkspaceDir,
				accountProbeOwner,
				"chown",
				"writable",
				"docker.run_args",
			} {
				assert.Containsf(t, err.Error(), want,
					"the refusal must name %q whatever the probe class; err=%v", want, err)
			}
			// The probe failure's text survives only as prose inside the
			// "(%s)" clause of the refusal — provenance: the daemon's errno
			// would be inert to errors.Is (the *exec.ExitError is formatted
			// into the message via %v, not joined via %w), which is exactly
			// why the op class must unlock dockerRootFSFullEvidence at
			// provision() to re-derive the typed errno. Asserting the prose
			// is present proves the operator still sees the cause; the
			// op-class assertion above proves provision() can attach the
			// typed errno the classifier reads.
			assert.Containsf(t, err.Error(), tc.wantProse,
				"the probe failure's text must surface as prose in the refusal; err=%v", err)
			requireNoRootChownOn(t, calls, dockerWorkspaceDir)
			assert.Empty(t, p.containerUser, "a refused provisioning must not switch the exec user")
		})
	}
}

// TestPrepareAccountUser_UnwritableHomeMountRefusesNamingTheMount covers the
// symmetric target: a mount at /af-home the account owner cannot write
// refuses with the mount and remedy named too.
func TestPrepareAccountUser_UnwritableHomeMountRefusesNamingTheMount(t *testing.T) {
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(accountChownScenarioStub(&calls, accountProbeOwner, accountChownScenario{
		mountPoints: []string{dockerAccountRuntimeHome},
		writable:    false,
	})))
	p := newAccountChownProvisioner(t)
	err := p.prepareAccountUser()
	require.Error(t, err)
	for _, want := range []string{"HOME", dockerAccountRuntimeHome, "writable", "docker.run_args"} {
		assert.Containsf(t, err.Error(), want, "the refusal must name %q; err=%v", want, err)
	}
	requireNoRootChownOn(t, calls, dockerAccountRuntimeHome)
	assert.Empty(t, p.containerUser)
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

// TestRootScriptChowns_CatchesOperandPositions pins the detector the #7563
// R5 assertions run on: /workspace flagged wherever a chown can reach it —
// directly after the owner, after another operand, inside find's -exec,
// spelled with a path-qualified chown, via a recursive ancestor operand, or
// via a find start point at the target-or-ancestor (#7571 review R10) —
// while a /workspace that is only find's -path prune argument, mkdir's
// operand, beneath a pruned mount, or inside a non-recursive ancestor
// operand is not a chown of /workspace and must not trip it.
func TestRootScriptChowns_CatchesOperandPositions(t *testing.T) {
	owner := accountProbeOwner
	for name, tc := range map[string]struct {
		script string
		want   bool
	}{
		"unguarded mutant, workspace last": {
			script: "mkdir -p " + shellQuote(dockerAccountRuntimeHome) + " " + shellQuote(dockerWorkspaceDir) +
				" && chown -h " + owner + " " + shellQuote(dockerAccountRuntimeHome) + " " + shellQuote(dockerWorkspaceDir),
			want: true,
		},
		"unguarded mutant, workspace first": {
			script: "chown -h " + owner + " " + shellQuote(dockerWorkspaceDir) + " " + shellQuote(dockerAccountRuntimeHome),
			want:   true,
		},
		"guarded workspace chown alone": {
			script: "mkdir -p " + shellQuote(dockerAccountRuntimeHome) + " && chown -h " + owner + " " + shellQuote(dockerWorkspaceDir),
			want:   true,
		},
		"workspace skipped, home chowned": {
			script: "mkdir -p " + shellQuote(dockerAccountRuntimeHome) + " " + shellQuote(dockerWorkspaceDir) +
				" && chown -h " + owner + " " + shellQuote(dockerAccountRuntimeHome),
			want: false,
		},
		"workspace is find's prune, not chown's operand": {
			// The recursive HOME chown prunes mounts under home; a /workspace
			// -path entry is find's operand, never chown's, and the session
			// must not read it as a chown of /workspace.
			script: "find '/root' \\( -path '/root/mnt' \\) -prune -o -exec chown -h " + owner + " {} + && " +
				"chown -h " + owner + " " + shellQuote(dockerAccountRuntimeHome),
			want: false,
		},
		"workspace chowned via find -exec": {
			script: "find '/root' -exec chown -h " + owner + " " + shellQuote(dockerWorkspaceDir) + " \\;",
			want:   true,
		},
		"workspace only as mkdir operand": {
			script: "mkdir -p " + shellQuote(dockerWorkspaceDir),
			want:   false,
		},
		// ---- #7571 review R10: basename match + ancestor-aware reach ----
		"path-qualified chown still counts": {
			script: "/bin/chown -h " + owner + " " + shellQuote(dockerWorkspaceDir),
			want:   true,
		},
		"recursive chown of the filesystem root reaches it": {
			script: "chown -R -h " + owner + " '/'",
			want:   true,
		},
		"recursive chown of an ancestor reaches it": {
			// '/' is /workspace's only ancestor; the --recursive spelling
			// counts too.
			script: "chown --recursive " + owner + " '/'",
			want:   true,
		},
		"non-recursive ancestor operand does not reach it": {
			// chown -h on '/' touches '/' alone — /workspace is unaffected.
			script: "chown -h " + owner + " '/'",
			want:   false,
		},
		"recursive chown of a sibling does not reach it": {
			script: "chown -R -h " + owner + " " + shellQuote(dockerAccountRuntimeHome),
			want:   false,
		},
		"find start point at the target reaches it": {
			script: "find " + shellQuote(dockerWorkspaceDir) + " -exec chown -h " + owner + " {} +",
			want:   true,
		},
		"find start point at an ancestor reaches it": {
			script: "find '/' -exec chown -h " + owner + " {} +",
			want:   true,
		},
		"find start point pruned at the target does not": {
			// The guard's own shape: the mount point under HOME is a -path
			// prune entry, so the traversal never chowns it.
			script: "find '/root' \\( -path " + shellQuote(dockerWorkspaceDir) + " \\) -prune -o -exec chown -h " + owner + " {} +",
			want:   false,
		},
		"find start point pruned at a mount ancestor does not": {
			script: "find '/' \\( -path '/workspace' \\) -prune -o -exec chown -h " + owner + " {} +",
			want:   false,
		},
		"find without -exec chown does not": {
			script: "find " + shellQuote(dockerWorkspaceDir) + " -name '*.tmp'",
			want:   false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, rootScriptChowns(tc.script, dockerWorkspaceDir),
				"script=%v", tc.script)
		})
	}
}

// #7571 review P2, pinned on the account path where the probe runs: the
// writability probe for a mounted target must run AFTER the permitted
// ancestor chowns — a /workspace symlinked to /af-home/workspace under a
// root-owned /af-home is unwritable to the account user until the HOME
// chown has re-owned /af-home, which is exactly the session the ordering
// keeps working. The probe running first would refuse a mount the just-
// re-owned ancestor makes traversable.
func TestPrepareAccountUser_MountedDescendantIsProbedAfterAncestorChown(t *testing.T) {
	var warnings bytes.Buffer
	log.CaptureOutput(t, log.WarningLog, "", &warnings)
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) == 0 {
			return nil, nil
		}
		joined := strings.Join(args, " ")
		switch {
		case args[0] != "exec":
			return nil, nil
		case strings.Contains(joined, "stat -c %u:%g"):
			return []byte(accountProbeOwner + "\n"), nil
		case isAccountReadMountTableExec(args):
			// /workspace is a symlink to /af-home/workspace, which is itself
			// a mount; /af-home is rootfs and is re-owned by the HOME chown.
			return []byte(dockerAccountRuntimeHome + "\n" + dockerAccountRuntimeHome + "/workspace\n" +
				mountinfoLine(1, "/", "overlay") + "\n" +
				mountinfoLine(2, dockerAccountRuntimeHome+"/workspace", "ext4") + "\n"), nil
		case strings.Contains(joined, "test -w"):
			// By the time the probe runs the stub's chown has "succeeded",
			// so /af-home is traversable: the mounted dir is writable.
			return nil, nil
		default:
			return nil, nil
		}
	}))
	p := newAccountChownProvisioner(t)
	require.NoError(t, p.prepareAccountUser(),
		"a user-writable mount beneath a HOME af just re-owned must keep working")
	requireNoRootChownOn(t, calls, dockerAccountRuntimeHome+"/workspace")
	// The HOME chown exec must land before the writability probe.
	chownIdx, probeIdx := -1, -1
	for i, call := range calls {
		joined := strings.Join(call, " ")
		if chownIdx < 0 && strings.Contains(joined, "--user 0:0") && strings.Contains(joined, "chown") {
			chownIdx = i
		}
		if strings.Contains(joined, "test -w") {
			probeIdx = i
		}
	}
	require.NotEqual(t, -1, chownIdx, "the HOME chown exec must run")
	require.NotEqual(t, -1, probeIdx, "the writability probe must run for the mounted workspace")
	assert.Less(t, chownIdx, probeIdx,
		"the ancestor chown must execute before the mounted descendant is probed; calls=%v", calls)
	assert.Contains(t, warnings.String(), "not chowning it")
}
