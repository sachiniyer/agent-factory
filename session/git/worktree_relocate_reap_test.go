package git

import (
	"bytes"
	"context"
	stdlog "log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"
)

// relocationWriterFixture starts a real survivor inside dir and returns its exit
// channel. It reuses startWorktreeWriter — the #2025 removal-side fixture — for
// the property that makes the assertion sound: the writer beats a heartbeat to an
// absolute path OUTSIDE the worktree, so it can only ever exit by being
// SIGNALLED, never because its cwd was moved out from under it. A closed exit
// channel therefore means the relocation reaped it, not that the move made a
// shell self-terminate.
//
// It also proves the writer is genuinely running before the relocation starts, so
// a pass cannot come from racing an idle process that had already exited.
func relocationWriterFixture(t *testing.T, dir string) (exited <-chan struct{}, heartbeat string) {
	t.Helper()
	heartbeat = filepath.Join(t.TempDir(), "heartbeat")
	exited = startWorktreeWriter(t, dir, heartbeat)
	requireEventually(t, 5*time.Second, func() bool {
		_, err := os.Stat(heartbeat)
		return err == nil
	}, "the survivor never started running inside "+dir)
	return exited, heartbeat
}

// requireReaped waits, bounded, for the writer to exit. The bound is generous
// versus the reap's SIGTERM→SIGKILL escalation yet finite, so a red test fails
// fast instead of hanging.
func requireReaped(t *testing.T, exited <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// TestArchiveWorktree_ReapsWritersBeforeMovingTheTree is the #3391 regression.
//
// Archive moved the worktree without reaping the processes writing inside it, so
// a build that outlived its pane kept running against a pathname that no longer
// held the tree: it died with an ENOENT that reads as a broken build (on a
// Dependabot review, a wrong verdict on a dependency bump) and re-created the
// vacated path by absolute name. Removal has had this reap since #2025 because a
// live writer makes a recursive delete fail loudly; a move succeeds silently,
// which is exactly why the gap went unnoticed for so long.
//
// Reaping is the right answer rather than refusing: archive's teardown confirms
// every pane dead BEFORE the worktree step (#802/#1917), so a process still
// writing here has escaped its pane, its terminal is destroyed, nothing is
// reading its output, and it can no longer finish usefully — only corrupt.
func TestArchiveWorktree_ReapsWritersBeforeMovingTheTree(t *testing.T) {
	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	exited, _ := relocationWriterFixture(t, srcPath)

	require.NoError(t, gw.ArchiveWorktree(dest))

	requireReaped(t, exited, "archive did not reap the writer working inside the worktree (#3391): "+
		"the move succeeds silently and leaves that process pointed at a pathname which no longer holds the tree")
	assert.False(t, pathExists(srcPath), "the vacated pathname must not survive the archive")
	assertLiveWorktreeAt(t, gw, dest)
}

// TestRestoreWorktreeTo_ReapsWritersBeforeMovingTheTree pins that the reap
// belongs to the MOVE rather than to the archive role. Restore relocates the same
// bytes back and carries the same hazard, so the discipline lives at the one
// shared point in the relocation engine — attaching it to a single call site is
// how #3391 came to exist in the first place.
func TestRestoreWorktreeTo_ReapsWritersBeforeMovingTheTree(t *testing.T) {
	gw, _, srcPath := archiveTestWorktree(t)
	archived := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, gw.ArchiveWorktree(archived))

	exited, _ := relocationWriterFixture(t, archived)

	require.NoError(t, gw.RestoreWorktreeTo(srcPath))

	requireReaped(t, exited, "restore did not reap the writer working inside the archived worktree")
	assert.False(t, pathExists(archived), "the vacated archive pathname must not survive the restore")
	assertLiveWorktreeAt(t, gw, srcPath)
}

// TestArchiveWorktree_ReportsResidueAtTheVacatedPath is the second half of
// #3391. A writer that outlives the reap re-creates the vacated pathname by
// absolute name (turbo's own `mkdir -p apps/…`), leaving a `.git`-less skeleton
// exactly where a later session deriving the same path would land — and both
// vacated paths from the live incidents are still on disk today because nothing
// ever noticed. The archive itself stays a success, because archive is the
// non-destructive lifecycle action, so the leftover must be reported rather than
// swallowed or promoted to an error.
func TestArchiveWorktree_ReportsResidueAtTheVacatedPath(t *testing.T) {
	prev := worktreeMoveFast
	worktreeMoveFast = func(g *GitWorktree, src, dest string) error {
		if err := prev(g, src, dest); err != nil {
			return err
		}
		return os.MkdirAll(filepath.Join(src, "apps", "frontend", ".next"), 0o755)
	}
	t.Cleanup(func() { worktreeMoveFast = prev })

	var warnings bytes.Buffer
	origWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = origWarning })

	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	require.NoError(t, gw.ArchiveWorktree(dest),
		"residue at the vacated path must not turn a working archive into a failure")
	assertLiveWorktreeAt(t, gw, dest)

	assert.Contains(t, warnings.String(), "that pathname exists again",
		"residue at the vacated pathname must be reported, not left silent")
	assert.NotContains(t, warnings.String(), "complete",
		"a cross-device archive may deliberately omit unreadable entries, so this warning must not assert completeness")
	assert.Contains(t, warnings.String(), srcPath)
	assert.Contains(t, warnings.String(), shellsuggest.Command("rm", "-rf", srcPath),
		"the report must name the by-hand cleanup for the leftover it found")
	assert.True(t, pathExists(srcPath),
		"af vacated this pathname, so whatever exists there now was created by something else: report, never remove")
}

// TestArchiveWorktree_SilentWhenTheVacatedPathStaysGone: the residue check must
// not fire on the ordinary archive. A warning on every successful archive would
// be worse than no check at all — it is the shape that trains an operator to
// ignore the one that matters.
func TestArchiveWorktree_SilentWhenTheVacatedPathStaysGone(t *testing.T) {
	var warnings bytes.Buffer
	origWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = origWarning })

	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	require.NoError(t, gw.ArchiveWorktree(dest))

	assert.False(t, pathExists(srcPath))
	assert.Empty(t, warnings.String(), "a clean archive must report nothing")
}

// TestReapRelocationSourceWriters_SkipsAnInPlaceRelocation: src == dest is the
// registration-repair-only path. Nothing is vacated, so nothing may be reaped,
// and the empty return is what tells the caller no residue check is owed.
//
// Liveness is asserted from the heartbeat ADVANCING rather than from the exit
// channel merely not being closed: "still running" is the claim, and a stalled
// process would satisfy the weaker form.
func TestReapRelocationSourceWriters_SkipsAnInPlaceRelocation(t *testing.T) {
	gw, _, _ := archiveTestWorktree(t)
	dir := t.TempDir()
	exited, heartbeat := relocationWriterFixture(t, dir)

	vacated, err := gw.reapRelocationSourceWriters(dir, dir, RelocationClaim{})
	require.NoError(t, err)
	assert.Empty(t, vacated, "an in-place relocation vacates no pathname")

	before, err := os.Stat(heartbeat)
	require.NoError(t, err)
	requireEventually(t, 5*time.Second, func() bool {
		after, statErr := os.Stat(heartbeat)
		return statErr == nil && after.ModTime().After(before.ModTime())
	}, "a relocation that moves nothing must leave the processes working there running")

	select {
	case <-exited:
		t.Fatal("a relocation that moves nothing must not reap the processes working there")
	default:
	}
}

// TestArchiveWorktree_DoesNotKillTmuxServer re-pins the #3186 exclusion through
// the call path this change adds.
//
// The shared tmux server inherits its cwd from the client that first started it,
// so it can legitimately be sitting inside the worktree being archived — and
// signalling it would tear down every session on the box. The exclusion lives
// inside the reaper's selector and was previously only reachable from the remove
// path; archive is now a second caller, and the consequence of the exclusion
// failing here is a fleet-wide outage rather than one lost build.
//
// The fixture is deliberately not a real tmux server: it is one disposable sleep
// process wearing tmux's server argv shape, with a cwd in the worktree. The
// production predicate reads argv, so this exercises the real identification and
// kill-set path without touching the user's tmux.
//
// Read it together with TestArchiveWorktree_ReapsWritersBeforeMovingTheTree,
// which puts an ORDINARY writer in exactly this position and requires it to die.
// The pair is a differential: argv is the only difference between them, so a
// survival here means the exclusion fired, not that the reap never ran.
func TestArchiveWorktree_DoesNotKillTmuxServer(t *testing.T) {
	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	cmd := exec.Command("sleep", "300")
	cmd.Args[0] = "tmux: server"
	cmd.Dir = srcPath
	require.NoError(t, cmd.Start())
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	process, err := proctree.Lookup(cmd.Process.Pid)
	require.NoError(t, err)
	root := normalizeWorktreePath(srcPath)
	requireEventually(t, 5*time.Second, func() bool {
		cwd, ok := proctree.WorkingDir(process.PID)
		return ok && pathAtOrUnder(root, filepath.Clean(cwd))
	}, "the tmux-server fixture cwd never became observable inside the worktree")

	require.NoError(t, gw.ArchiveWorktree(dest))

	require.True(t, proctree.AliveSame(process),
		"a tmux server whose cwd is inside an archived worktree must not enter the kill set (#3186)")
	assertLiveWorktreeAt(t, gw, dest)
}

// TestArchiveWorktree_RefusesToReapAReplacedSource is the #3278 rule applied to
// the relocation reap.
//
// Selecting processes by PATHNAME is destructive, so it needs its ownership
// proof at the FRONT. Source resolution is a point-in-time claim, and the
// engine's next revalidation happens at the fast-move boundary — after this reap
// would already have signalled. Without the proof here, a same-uid actor who
// swaps the claimed directory in that window has the processes working inside
// *its* directory killed, and the mismatch is only noticed afterwards.
func TestArchiveWorktree_RefusesToReapAReplacedSource(t *testing.T) {
	gw, _, _ := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	var replacementWriter <-chan struct{}
	swapped := false
	prev := relocateBeforeWriterReap
	relocateBeforeWriterReap = func(path string) {
		if swapped {
			return
		}
		swapped = true
		// A same-uid racer strands the claimed tree and drops its own directory,
		// with its own live process, at the pathname the reap is about to use.
		require.NoError(t, os.Rename(path, path+".stranded"))
		require.NoError(t, os.Mkdir(path, 0o755))
		replacementWriter, _ = relocationWriterFixture(t, path)
	}
	t.Cleanup(func() { relocateBeforeWriterReap = prev })

	err := gw.ArchiveWorktree(dest)

	require.True(t, swapped, "the test must have replaced the source before the reap")
	require.Error(t, err, "a source that no longer matches the claim must not be relocated")
	require.NotNil(t, replacementWriter)
	select {
	case <-replacementWriter:
		t.Fatal("the reap signalled processes inside a replacement directory the claim no longer identifies (#3278)")
	case <-time.After(500 * time.Millisecond):
	}
}

// TestArchiveWorktree_ReportsResidueAtTheRecoveryAlternatePath covers the one
// success path that reaches no move at all.
//
// When a bounded `git worktree move` is cut off AFTER moving the directory, the
// retry resolves the destination as the claimed source and returns through the
// repair-only branch — before anything computes a vacated pathname. The original
// pathname was still vacated by the first attempt, so the residue obligation has
// to come from the candidate the claim did NOT select.
func TestArchiveWorktree_ReportsResidueAtTheRecoveryAlternatePath(t *testing.T) {
	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, gw.ArchiveWorktree(dest))

	// A writer that outlived the first attempt re-created the vacated pathname.
	require.NoError(t, os.MkdirAll(filepath.Join(srcPath, "apps"), 0o755))

	identity, err := relocationPathIdentity(dest)
	require.NoError(t, err)
	gw.beginRelocationRecovery(dest, srcPath, identity)

	var warnings bytes.Buffer
	origWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = origWarning })

	require.NoError(t, gw.ArchiveWorktree(dest))

	assert.Contains(t, warnings.String(), "that pathname exists again")
	assert.Contains(t, warnings.String(), srcPath,
		"the recovery retry must report residue at the candidate it did not select")
}

// TestReportRelocationResidue_UnreadableProbeIsNotAssumedClear: a check that
// could not run must not read as a check that passed. Only a genuine "does not
// exist" is silence.
func TestReportRelocationResidue_UnreadableProbeIsNotAssumedClear(t *testing.T) {
	vacated := filepath.Join(t.TempDir(), "vacated")
	restore := SetRelocationIdentityErrorForTest(vacated, context.DeadlineExceeded)
	t.Cleanup(restore)

	var warnings bytes.Buffer
	origWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = origWarning })

	reportRelocationResidue(vacated)

	assert.Contains(t, warnings.String(), "could not be established",
		"a failed read is not an empty result")
	assert.Contains(t, warnings.String(), vacated)
}

// TestReportRelocationResidue_BoundsTheProbe: this check runs after the bytes and
// the git registration are committed but before the relocation returns, so an
// unbounded stat against a filesystem that stalled during the move would wedge
// teardown with its result established and nothing able to finalize it — the
// exact class #1917 bounded the rest of this path for.
func TestReportRelocationResidue_BoundsTheProbe(t *testing.T) {
	prevTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relocationIdentityTimeout = prevTimeout })

	vacated := filepath.Join(t.TempDir(), "stalled")
	release := make(chan struct{})
	prevIdentity := relocationPathIdentity
	relocationPathIdentity = func(observed string) (pathIdentity, error) {
		if observed == vacated {
			<-release // an unresponsive mount: the syscall never returns
			return pathIdentity{}, nil
		}
		return prevIdentity(observed)
	}
	t.Cleanup(func() {
		relocationPathIdentity = prevIdentity
		close(release)
	})

	var warnings bytes.Buffer
	origWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = origWarning })

	done := make(chan struct{})
	go func() {
		reportRelocationResidue(vacated)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the residue probe was not bounded: a stalled filesystem wedges it after the relocation committed")
	}

	assert.Contains(t, warnings.String(), "could not be established")
}
