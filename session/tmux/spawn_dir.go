package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/sachiniyer/agent-factory/log"
)

// The spawn-directory invariant (#5172): af NEVER starts a session pane, agent,
// or tab with a working directory other than the one its caller requested.
// tmux's `new-session -c` does not report that contract back — an unusable -c
// falls back to the server's own cwd with no error — so the check lives at the
// spawn seam itself, before and after the new-session command, where no caller
// can bypass it.
//
// Two classifications, mirroring the codebase's known/unknown convention:
//
//   - Missing (ErrSpawnDirMissing): the requested directory is conclusively
//     unusable — absent, empty, not a directory, or unreachable through a
//     non-directory component (ENOTDIR) — or the spawned pane provably landed
//     elsewhere. Every restore/respawn route maps this to the lost row with
//     the WORKTREE_MISSING reason.
//   - Unknown (ErrSpawnDirUnknown): the requested directory's state could
//     not be established — a stat error other than ENOENT. Not evidence the
//     worktree is gone: the row is held and retried on a later pass rather
//     than declared missing. Post-spawn evidence that is merely unavailable
//     (a tmux too old to answer pane_start_path) is not Unknown either —
//     verifySpawnedPaneDir skips it.

// statSpawnDir is os.Stat renamed for this seam. A var so tests can stage
// non-ENOENT stat failures — permission and I/O errors — that a real
// filesystem cannot produce portably (a root test runner ignores chmod).
var statSpawnDir = os.Stat

// checkSpawnDir refuses a spawn whose start directory is unusable, before any
// tmux command has run. Callers invoke it while the session name is already
// known-absent, so the refusal can carry ErrSessionNotStarted: nothing was
// ever spawned. On success it returns the admitted directory's FileInfo so the
// post-spawn check compares the pane against THAT inode — not a possibly
// renamed-and-recreated path sampled later (#5174 review).
func checkSpawnDir(workDir string) (os.FileInfo, error) {
	if workDir == "" {
		// An empty -c asks tmux for its default — the server's cwd — which is
		// exactly the fallback this seam exists to forbid.
		return nil, fmt.Errorf("%w: %w: %w: no start directory was requested",
			ErrSessionNotStarted, ErrSpawnDirMissing, os.ErrNotExist)
	}
	info, err := statSpawnDir(workDir)
	switch {
	case err == nil && info.IsDir():
		return info, nil
	case err == nil:
		// A non-directory at the worktree path is not a working directory;
		// tmux would fall back exactly as if it were absent.
		return nil, fmt.Errorf("%w: %w: %w: %s exists but is not a directory",
			ErrSessionNotStarted, ErrSpawnDirMissing, os.ErrNotExist, workDir)
	case errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		// ENOTDIR is an intermediate-component form of the same verdict: a
		// file where a directory belongs makes the path conclusively
		// unusable, not merely unverifiable — the row goes Lost and enters
		// worktree recovery instead of being held and retried forever.
		// os.ErrNotExist is wrapped explicitly: ENOTDIR does not Is() to it,
		// and the daemon's WORKTREE_MISSING classification keys on ENOENT.
		return nil, fmt.Errorf("%w: %w: %s: %w: %w",
			ErrSessionNotStarted, ErrSpawnDirMissing, workDir, os.ErrNotExist, err)
	default:
		return nil, fmt.Errorf("%w: %w: cannot verify %s: %w",
			ErrSessionNotStarted, ErrSpawnDirUnknown, workDir, err)
	}
}

// spawnedPaneDirUnusableLogged logs the post-spawn check's skip once per
// process — an environment where no source answers would otherwise repeat
// it on every spawn.
var spawnedPaneDirUnusableLogged sync.Once

// verifySpawnedPaneDir asks where the freshly spawned pane is running and
// tears the session down on a POSITIVE mismatch — judged against `want`, the
// directory identity checkSpawnDir admitted, never a re-stat of the path. The
// pinned identity is what keeps a rename-and-recreate race from re-baselining
// the check onto a directory the spawn was never validated against (#5174).
// It runs after the existence poll, so the session this Start created is
// known-present: "the session exists" is not proof the pane started inside it.
//
// Information that is merely unavailable never tears anything down — a tmux
// that cannot say where the pane is or an answer that does not resolve is
// skipped in favour of the next source, and if every source is silent the
// check is skipped with a one-time INFO log. Sources, in order:
//
//   - /proc/<#{pane_pid}>/cwd: the pane root's live cwd — the kernel's own
//     record of where the process runs, readable through procfs even after
//     the directory itself was unlinked (Linux). The strongest source:
//     it cannot echo the request and cannot be rewritten by tmux. Skipped
//     when #{pane_dead} says tmux marked the pane dead: a reaped root's
//     stale pid stays on record and the kernel may have recycled it.
//   - #{pane_current_path}: where the pane is now; every supported tmux
//     answers it, at the price of an early-chdir program moving the answer.
//   - #{pane_start_path}: weakest, kept last. On tmux >= 3.4 it records the
//     -c tmux was HANDED, not where the pane landed — the #5174 play-test
//     showed it echoing the requested worktree while the process actually
//     ran in the fallback cwd — so it can only confirm the request, never
//     contradict it, and a rename-and-replace of the admitted dir can make
//     it resolve outside without the pane having moved: a match may
//     confirm, a mismatch may never convict (#5174 review). On tmux < 3.4
//     it expands empty.
//
// A match is the admitted inode or an inode WALK up to it (paneDirInside), not
// a path string: a process tab legitimately running `cd frontend && npm run
// dev` lands inside its own worktree and is a correct spawn, while a tmux
// fallback to the server cwd or $HOME is never inside the tree. Residual: a
// command that chdirs OUT of its worktree in the window before this check
// reads is indistinguishable from a tmux fallback by every source available —
// pane_start_path is an echo of the request, and /proc and current_path both
// show the post-chdir location — so it is torn down the same way; the
// worktree-pinning rule cannot tell "agent left its tree" from "tmux never
// entered it", and refusing is the only answer that keeps the invariant.
//
// ErrSessionNotStarted joins a returned error only when teardown of the
// misplaced pane is conclusive, the same contract the readiness-timeout
// path keeps.
func (t *TmuxSession) verifySpawnedPaneDir(workDir string, want os.FileInfo) error {
	// The re-stat below exists only to name a mid-spawn disappearance — the
	// comparison never consults it; `want` alone decides the match.
	switch info, wantErr := statSpawnDir(workDir); {
	case errors.Is(wantErr, os.ErrNotExist) || errors.Is(wantErr, syscall.ENOTDIR):
		// The directory checkSpawnDir admitted moments ago is gone — the
		// pane necessarily started in tmux's fallback, wherever that is.
		// Proven wrong, not unproven. ENOTDIR rides along: an intermediate
		// component becoming a file is the same conclusive disappearance —
		// and like checkSpawnDir it must still wrap os.ErrNotExist itself,
		// since ENOTDIR does not Is() to it.
		return t.resolveSpawnedPaneDir(fmt.Errorf(
			"%w: start directory %s vanished during spawn: %w: %w",
			ErrSpawnDirMissing, workDir, os.ErrNotExist, wantErr))
	case wantErr == nil && !info.IsDir():
		// A non-directory at the worktree path is not a working directory.
		return t.resolveSpawnedPaneDir(fmt.Errorf(
			"%w: start directory %s is no longer a directory",
			ErrSpawnDirMissing, workDir))
	}
	// wantErr == nil (dir present) or an unverifiable stat — either way the
	// sources below still compare against the pinned inode, so proceed.

	inside, known, source, observed := t.observedPanePlacement(want)
	switch {
	case inside:
		t.setMisplacedPane(false)
		return nil
	case !known:
		spawnedPaneDirUnusableLogged.Do(func() {
			log.InfoLog.Printf("session %s: post-spawn cwd check skipped: no tmux source could report the spawned pane's working directory",
				t.sanitizedName)
		})
		return nil
	default:
		return t.resolveSpawnedPaneDir(fmt.Errorf(
			"%w: pane for session %s started in %s, not the requested %s (via %s)",
			ErrSpawnDirMissing, t.sanitizedName, observed, workDir, source))
	}
}

// verifyReattachPaneDir applies the spawn's placement check to a session af
// did NOT just create: the rebind half of the #5172 invariant. A spawn that
// proved misplaced and whose teardown failed leaves exactly this shape — a
// live session name with a pane outside its worktree — and a reattach that
// trusted name evidence alone would bind it and mark the row ready in the
// wrong directory (#5174 review). The verdict is re-derived on every rebind
// rather than remembered: in-memory rejection dies with the daemon, and the
// first post-restart reattach is exactly the path that must refuse.
//
// Unlike the spawn check this refuses the BIND, not the pane — it kills
// nothing it did not create. Missing or unverifiable evidence never
// refuses: an unstat-able worktree baseline (a pane legitimately holding a
// deleted dir open survives its unlink, so absence cannot convict it) and
// silent sources both mean "cannot tell", not "misplaced". Residual: a
// command that chdir'd OUT of its worktree before this check reads is
// indistinguishable from a never-placed pane — every source reports the
// post-chdir location — and is refused the same way; no evidence can
// separate them, so refusal keeps the invariant.
func (t *TmuxSession) verifyReattachPaneDir(workDir string) error {
	want, err := statSpawnDir(workDir)
	if err != nil || !want.IsDir() {
		return nil
	}
	inside, known, source, observed := t.observedPanePlacement(want)
	switch {
	case inside:
		// A positive inside verdict supersedes an earlier refusal: the pane
		// provably sits in its worktree NOW (it may have been moved back, or
		// the earlier observation read a since-replaced inode). Latches hold
		// only what was last proven.
		t.setMisplacedPane(false)
		return nil
	case !known:
		// Nothing could place the pane — neither convict it nor clear a
		// previous conviction. The last positive verdict stands.
		return nil
	default:
		// Latch the verdict so a name-only liveness probe cannot promote the
		// row back to Ready while the refused pane still holds the name
		// (#5174 review — backend_local.IsAlive consults it).
		t.setMisplacedPane(true)
		return fmt.Errorf(
			"%w: pane for session %s sits in %s, not the persisted %s (via %s); refusing to reattach a misplaced pane",
			ErrSpawnDirMissing, t.sanitizedName, observed, workDir, source)
	}
}

// observedPanePlacement reads the placement sources in truth-first order —
// /proc/<pane_pid>/cwd, then #{pane_current_path}, then #{pane_start_path} —
// and reports the first CONCLUSIVE verdict against the admitted inode:
// observed+source name the directory that produced it, and inside says
// whether that directory is the admitted inode or a descendant. known=false
// means no source could place the pane at all — unsupported fields, empty
// expansions, unstat-able paths, unresolvable observations, command silence
// — which is never evidence in either direction.
//
// A display-message DEADLINE is not a per-format answer: it is the server
// not answering at all, and every remaining format would pay the same
// tmuxCommandTimeout for the same silence. The first timeout stops the walk
// rather than stacking one deadline per source inside Start (#5174 review).
func (t *TmuxSession) observedPanePlacement(want os.FileInfo) (inside, known bool, source, observed string) {
	sources := []struct {
		name string
		read func() (string, bool)
		// convict is whether a resolved outside-the-worktree answer may
		// produce the negative verdict. pane_start_path is denied it: tmux
		// stores the -c it was HANDED (spawn.c keeps the request, not the
		// realized chdir), so its answer names intent, not placement. If the
		// admitted directory was renamed-and-replaced — or a symlink on the
		// path was retargeted — after tmux entered the original inode, the
		// stored path resolves to the replacement while the pane remains
		// correctly bound to the original. The echo can confirm a match but
		// can never convict (#5174 review).
		convict bool
	}{
		{"proc-cwd", t.paneProcCwd, true},
		{"pane_current_path", func() (string, bool) { return t.paneFormatField("#{pane_current_path}") }, true},
		{"pane_start_path", func() (string, bool) { return t.paneFormatField("#{pane_start_path}") }, false},
	}
	for _, s := range sources {
		actual, timedOut := s.read()
		if timedOut {
			break
		}
		if !filepath.IsAbs(actual) {
			// Empty (unsupported on this tmux) or unusable — try the next
			// source rather than treat silence as a mismatch.
			continue
		}
		if _, err := statSpawnDir(actual); err != nil {
			continue
		}
		in, resolvable := paneDirInside(actual, want)
		switch {
		case in:
			return true, true, s.name, actual
		case resolvable && s.convict:
			return false, true, s.name, actual
		}
		// Inconclusive — an observation that cannot be resolved to a
		// walkable ancestry (a procfs cwd deleted after the spawn: stat on
		// the link answers, EvalSymlinks cannot) whose own inode is not the
		// admitted one, or an answer from a source that may not convict.
		// It cannot prove inside OR outside, so it falls through to the
		// next source rather than convicting or clearing.
	}
	return false, false, "", ""
}

// paneDirInside reports whether dir is the admitted directory itself or a
// descendant of it, by walking dir's ancestors and comparing each stat'd
// inode against `want` — a path string is never compared, so an observed
// /x/wt-evil does not match an admitted /x/wt, and an inode swapped into the
// worktree path mid-spawn cannot launder a different directory into a match.
//
// dir is resolved once before the walk: an observed /proc/<pid>/cwd is a
// SYMLINK whose own ancestors (/proc/<pid> → /proc → /) never reach the
// worktree — only the resolved path's ancestors can (#5174 review). When
// the path does not resolve at all — procfs reports a cwd deleted after the
// spawn as "<path> (deleted)", which EvalSymlinks cannot touch — the kernel
// still answers stat on the link itself, so the identity compare remains
// real evidence but there is no ancestry to walk: inside reports the
// level-0 verdict alone and resolvable reports false, telling the caller
// "not inside" here means "cannot tell", not "outside".
func paneDirInside(dir string, want os.FileInfo) (inside, resolvable bool) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		info, serr := statSpawnDir(dir)
		return serr == nil && os.SameFile(info, want), false
	}
	for d := resolved; ; d = filepath.Dir(d) {
		if info, err := statSpawnDir(d); err == nil && os.SameFile(info, want) {
			return true, true
		}
		if parent := filepath.Dir(d); parent == d {
			return false, true
		}
	}
}

// paneFormatField answers one tmux display-message format for this session,
// or "" when the query fails or the field expands to nothing — which on a
// tmux that does not know the field is exactly how "unsupported" arrives.
// The second result reports a fired deadline: the SERVER's silence rather
// than a per-format failure, which the caller treats as "stop asking"
// rather than "try the next format" (#5174 review).
func (t *TmuxSession) paneFormatField(format string) (string, bool) {
	ctx, cancel := tmuxTimeoutContext()
	out, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t",
		exactTarget(t.sanitizedName), format)
	// Read the deadline BEFORE cancelling: cancel() stamps ctx.Err() with
	// Canceled unconditionally, so only a ctx.Err() taken here can still
	// distinguish "the command's own error" from "the context fired".
	ctxErr := ctx.Err()
	cancel()
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return "", true
	}
	if err != nil || ctxErr != nil {
		return "", false
	}
	// Strip only the command's line terminator — never whitespace: a start
	// directory whose final component legitimately ends in whitespace would
	// otherwise stat a truncated, DIFFERENT path and be torn down on the
	// mismatch.
	return strings.TrimSuffix(strings.TrimSuffix(string(out), "\r\n"), "\n"), false
}

// procfsRoot is the procfs mount point — a var so tests can point it at a
// fixture instead of the host's real process table.
var procfsRoot = "/proc"

// paneProcCwd returns /proc/<pane_pid>/cwd for the freshly spawned pane, or ""
// off Linux or when the pane pid is unknown. Stat-ing the path resolves
// procfs's symlink to the directory's identity — the kernel's record of where
// the process actually runs, not the -c it was asked for — so it still proves
// the truth even when the worktree itself was unlinked after the spawn. The
// second result propagates paneFormatField's server-silence signal.
func (t *TmuxSession) paneProcCwd() (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	// #{pane_dead} first, as its own single-field query — panePID's row
	// format is deliberately NOT reused here: it is also the teardown path's
	// pane identification, and conflating the two would let a placement
	// probe satisfy an ordering assertion meant for the kill. A pane held by
	// remain-on-exit keeps reporting its ORIGINAL pane_pid once tmux marks
	// it dead, and after the server reaps the root the kernel is free to
	// recycle that pid — /proc/<pid>/cwd would then be an UNRELATED
	// process's directory, convicting a correctly placed retained pane
	// before pane_current_path gets a say (#5174 review). Procfs is skipped
	// whenever tmux reports the pane dead: a still-uncollected root keeps
	// the pid, but pane_current_path's retained record answers equally well,
	// while a reaped root makes the pid unsafe. A tmux too old to know the
	// field expands it empty — not-dead — and the proc source stays.
	dead, timedOut := t.paneFormatField("#{pane_dead}")
	if timedOut {
		return "", true
	}
	if dead == "1" {
		return "", false
	}
	field, timedOut := t.paneFormatField("#{pane_pid}")
	if timedOut {
		return "", true
	}
	pid, err := strconv.Atoi(field)
	if err != nil || pid <= 0 {
		return "", false
	}
	return filepath.Join(procfsRoot, strconv.Itoa(pid), "cwd"), false
}

// resolveSpawnedPaneDir tears down the just-created session whose pane
// provably did not start in the requested directory — a pane af cannot place
// there never survives under a row that will report ready. It waits for the
// pane to exit, not just for kill-session to land: kill-session only delivers
// SIGHUP, and a process still flushing state inside the worktree must be gone
// before ErrSessionNotStarted authorizes the caller to delete that tree.
func (t *TmuxSession) resolveSpawnedPaneDir(classErr error) error {
	// Every caller reaches here on a positive misplaced-pane verdict — the
	// observed mismatch above or a start dir that vanished mid-spawn, which
	// makes the fallback landing certain. Latch it BEFORE the teardown so a
	// failed close cannot leave name evidence free to promote the row later
	// (#5174 review — backend_local.IsAlive consults it).
	t.setMisplacedPane(true)
	state, closeErr := t.CloseAndWaitForPaneExit()
	if state == PaneStateKnown && closeErr == nil {
		return fmt.Errorf("%w: %w", ErrSessionNotStarted, classErr)
	}
	if closeErr != nil {
		// %w, never %v: the classification sentinel must survive the
		// cleanup annotation or the daemon's WORKTREE_MISSING mapping
		// loses this path (#5174 review).
		return fmt.Errorf("%w (cleanup error: %v)", classErr, closeErr)
	}
	return classErr
}
