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

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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
//     unusable — absent, empty, not a directory, unenterable, or unreachable
//     through a non-directory component (ENOTDIR) — or the spawned pane
//     provably landed elsewhere. Every restore/respawn route maps this to
//     the lost row with the WORKTREE_MISSING reason.
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

// spawnDirEnterable answers the question tmux's own chdir(2) answers at
// spawn: can this process enter the directory? unix.Access asks under the
// real uid — the same credential chdir will apply — so a directory that
// stats fine but cannot be entered (a mode-6xx worktree, a permission-torn
// mount) is refused BEFORE tmux's fallback can claim the pane, and a dead
// pane's verdict can trust "still enterable ⇒ the pane entered" (#5174
// review). A var for the same reason statSpawnDir is one: a root test
// runner cannot stage EACCES with chmod.
var spawnDirEnterable = func(path string) error {
	return unix.Access(path, unix.X_OK)
}

// checkSpawnDir refuses a spawn whose start directory is unusable, before any
// tmux command has run. The error carries ONLY the directory classification —
// ErrSpawnDirMissing/ErrSpawnDirUnknown (+ os.ErrNotExist where the daemon's
// WORKTREE_MISSING mapping keys on it) — never ErrSessionNotStarted: the loose
// existence probe that ran first collapses non-answer failures into absence,
// so the cleanup-authorizing marker may only be attached by a caller that ran
// the strict probe and got tmux's determinate "not there" (#5174 review). On
// success it returns the admitted directory's FileInfo so the post-spawn check
// compares the pane against THAT inode — not a possibly renamed-and-recreated
// path sampled later.
func checkSpawnDir(workDir string) (os.FileInfo, error) {
	if workDir == "" {
		// An empty -c asks tmux for its default — the server's cwd — which is
		// exactly the fallback this seam exists to forbid.
		return nil, fmt.Errorf("%w: %w: no start directory was requested",
			ErrSpawnDirMissing, os.ErrNotExist)
	}
	info, err := statSpawnDir(workDir)
	switch {
	case err == nil && info.IsDir():
		// chdir(2) is tmux's only fallback trigger — the pane can land
		// outside this directory only when entering it fails. Stat proved
		// it exists; X_OK proves it enterable, under the real uid chdir
		// will apply — the one fact that lets a later verdict trust "the
		// pane landed inside" with no live pane evidence at all.
		switch aerr := spawnDirEnterable(workDir); {
		case aerr == nil:
			return info, nil
		case errors.Is(aerr, os.ErrPermission) || errors.Is(aerr, os.ErrNotExist) ||
			errors.Is(aerr, syscall.ENOTDIR):
			// Unenterable is conclusively unusable for a spawn — as surely
			// as absent: chdir CANNOT succeed into it, so tmux's fallback
			// is a certainty, not a risk (#5174 review).
			return nil, fmt.Errorf("%w: %w: %s exists but cannot be entered",
				ErrSpawnDirMissing, os.ErrNotExist, workDir)
		default:
			return nil, fmt.Errorf("%w: cannot verify %s is enterable: %w",
				ErrSpawnDirUnknown, workDir, aerr)
		}
	case err == nil:
		// A non-directory at the worktree path is not a working directory;
		// tmux would fall back exactly as if it were absent.
		return nil, fmt.Errorf("%w: %w: %s exists but is not a directory",
			ErrSpawnDirMissing, os.ErrNotExist, workDir)
	case errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		// ENOTDIR is an intermediate-component form of the same verdict: a
		// file where a directory belongs makes the path conclusively
		// unusable, not merely unverifiable — the row goes Lost and enters
		// worktree recovery instead of being held and retried forever.
		// os.ErrNotExist is wrapped explicitly: ENOTDIR does not Is() to it,
		// and the daemon's WORKTREE_MISSING classification keys on ENOENT.
		return nil, fmt.Errorf("%w: %s: %w: %w",
			ErrSpawnDirMissing, workDir, os.ErrNotExist, err)
	default:
		return nil, fmt.Errorf("%w: cannot verify %s: %w",
			ErrSpawnDirUnknown, workDir, err)
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
// A pane that exited before any live source could answer gets its verdict
// from the spawn itself instead: the exec shim chdirs into the admitted
// directory at the instant the command would run and stamps a refusal (exit
// code + scrollback marker) when it cannot — so pane_dead_status, or the
// marker where that field expands empty, is evidence tied to the spawn
// rather than a later point-in-time accessibility sample (#5174 review).
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
	// The sources speak first, always against `want` — the admitted inode —
	// never the re-stat of the path. A rename or unlink of the admitted
	// directory between checkSpawnDir and here leaves the PATHNAME dead but
	// the INODE live, and a pane that entered it stays correctly placed by
	// identity: only a positive source verdict may convict (#5174 review).
	inside, known, dead, source, observed := t.observedPanePlacement(want)
	if inside {
		return nil
	}
	if known {
		return t.resolveSpawnedPaneDir(fmt.Errorf(
			"%w: pane for session %s started in %s, not the requested %s (via %s)",
			ErrSpawnDirMissing, t.sanitizedName, observed, workDir, source))
	}
	// No source could place the pane — the pathname itself is the last
	// evidence. The directory checkSpawnDir admitted moments ago answering
	// ENOENT now means tmux's fallback landing is the only possibility the
	// silence can hide. ENOTDIR rides along: an intermediate component
	// becoming a file is the same conclusive disappearance — and like
	// checkSpawnDir it must still wrap os.ErrNotExist itself, since ENOTDIR
	// does not Is() to it.
	info, wantErr := statSpawnDir(workDir)
	switch {
	case errors.Is(wantErr, os.ErrNotExist) || errors.Is(wantErr, syscall.ENOTDIR):
		return t.resolveSpawnedPaneDir(fmt.Errorf(
			"%w: start directory %s vanished during spawn: %w: %w",
			ErrSpawnDirMissing, workDir, os.ErrNotExist, wantErr))
	case wantErr == nil && !info.IsDir():
		// A non-directory at the worktree path is not a working directory.
		return t.resolveSpawnedPaneDir(fmt.Errorf(
			"%w: start directory %s is no longer a directory",
			ErrSpawnDirMissing, workDir))
	}
	if dead {
		// tmux marked the pane dead before any live source could verify
		// it — the remain-on-exit process that finished inside the spawn
		// window. For a dead pane no live evidence can ever exist: procfs
		// is off (the pid may be recycled), current_path expands empty by
		// design, and start_path is the request's own echo. But placement
		// stays DECIDABLE, on evidence the spawn authored itself: the
		// exec shim chdirs into the requested directory at the instant the
		// command would run and, failing that, prints a refusal marker and
		// exits with its own status. A recorded refusal convicts outright:
		// the pane never ran inside the worktree, which a second
		// point-in-time accessibility check could never prove — an
		// enterability flap can be undone between the shim's attempt and
		// this read (#5174 review).
		//
		// The marker is what the verdict keys on, and pane_dead_status only
		// narrows when the scrollback is read: a real refusal ALWAYS shows
		// it, while a command that exited with the shim's code by
		// coincidence, or any other real exit status, does not. The field
		// expands empty on tmux builds that do not know it AND on a pane
		// killed by a signal, so "" also has to look. A timed-out status
		// query means the server is not answering — the scrollback query
		// would pay the same deadline — and the pathname arm below stays
		// the decider, the same non-evidence rule every source keeps.
		// shimPassed records that the shim's own verdict channel ANSWERED
		// and recorded no refusal: the pane's spawn-tied evidence says the
		// chdir succeeded and the command exec'd. A later accessibility
		// sample must never override that — a short process can correctly
		// start in the admitted inode and then drop the worktree's execute
		// bit before exiting (or an external chmod can land in the same
		// interval); reinterpreting that post-exit change as a historical
		// spawn failure tears down a pane that provably ran in the right
		// directory (#5174 review).
		shimPassed := false
		if status, statusTimedOut := t.paneFormatField("#{pane_dead_status}"); !statusTimedOut {
			if status != "" && status != strconv.Itoa(sessionenv.SpawnDirRefusalExitCode) {
				// Any other recorded exit status is the refusal channel
				// answering NO: the shim only exits SpawnDirRefusalExitCode
				// on a refused chdir, so a different status means the
				// command exec'd inside the admitted directory.
				shimPassed = true
			} else {
				// "" or the refusal code both require the scrollback to
				// corroborate — and the scrollback must have ANSWERED: a
				// capture that failed or timed out leaves the channel
				// unread, which is inconclusive, not "no refusal"
				// (#5174 review).
				refused, answered := t.paneShowsSpawnDirRefusal()
				if refused {
					return t.resolveSpawnedPaneDir(fmt.Errorf(
						"%w: %w: the launch shim could not enter %s — pane for session %s never ran inside it",
						ErrSpawnDirMissing, os.ErrNotExist, workDir, t.sanitizedName))
				}
				shimPassed = answered
			}
		}
		// With no refusal on record, what remains is only the
		// moved-worktree question: the pathname still has to name the
		// admitted inode for the finish surface to record a clean spawn.
		// A status query that never answered leaves the refusal channel
		// unread — nothing then established the shim passed, and accepting
		// on that silence is the pre-shim guess this arm exists to retire.
		// Neither inconclusive shape tears the pane down: a positive
		// mismatch is the ONLY license resolveSpawnedPaneDir has, and the
		// dead pane — correctly placed or not — holds the finish surface
		// the caller reads. The survivor keeps its name for the caller's
		// retention path instead (#5174 review).
		switch {
		case wantErr != nil:
			return fmt.Errorf(
				"%w: cannot verify %s after the pane exited: %w",
				ErrSpawnDirUnknown, workDir, wantErr)
		case !os.SameFile(info, want):
			return t.resolveSpawnedPaneDir(fmt.Errorf(
				"%w: %s no longer names the directory the pane for session %s was spawned into",
				ErrSpawnDirMissing, workDir, t.sanitizedName))
		case !shimPassed:
			return fmt.Errorf(
				"%w: pane for session %s exited but the spawn's refusal channel could not be read",
				ErrSpawnDirUnknown, t.sanitizedName)
		default:
			// The admitted inode still answers the path and the shim
			// provably entered it — the finished pane ran inside the
			// worktree regardless of any accessibility change since. Its
			// exit status and output stay for the caller's finish surface.
			return nil
		}
	}
	spawnedPaneDirUnusableLogged.Do(func() {
		log.InfoLog.Printf("session %s: post-spawn cwd check skipped: no tmux source could report the spawned pane's working directory",
			t.sanitizedName)
	})
	return nil
}

// observedPanePlacement reads the placement sources in truth-first order —
// /proc/<pane_pid>/cwd, then #{pane_current_path}, then #{pane_start_path} —
// and reports the first CONCLUSIVE verdict against the admitted inode:
// observed+source name the directory that produced it, and inside says
// whether that directory is the admitted inode or a descendant. known=false
// means no source could place the pane at all — unsupported fields, empty
// expansions, unstat-able paths, unresolvable observations, command silence
// — which is never evidence in either direction. dead reports the separate
// affirmative fact tmux gave up front: pane_dead == "1" means the pane's
// root already exited, so live evidence can never arrive — the caller's
// no-source arm must then decide on the pathname's own facts (still the
// admitted inode, still enterable) rather than accepting on silence
// (#5174 review).
//
// A display-message DEADLINE is not a per-format answer: it is the server
// not answering at all, and every remaining format would pay the same
// tmuxCommandTimeout for the same silence. The first timeout stops the walk
// rather than stacking one deadline per source inside Start (#5174 review).
func (t *TmuxSession) observedPanePlacement(want os.FileInfo) (inside, known, dead bool, source, observed string) {
	// #{pane_dead} first, as its own single-field query — panePID's row
	// format is deliberately NOT reused here: it is also the teardown path's
	// pane identification, and conflating the two would let a placement
	// probe satisfy an ordering assertion meant for the kill. A pane held by
	// remain-on-exit keeps reporting its ORIGINAL pane_pid once tmux marks
	// it dead, and after the server reaps the root the kernel is free to
	// recycle that pid — /proc/<pid>/cwd would then be an UNRELATED
	// process's directory, convicting a correctly placed retained pane
	// before pane_current_path gets a say (#5174 review). And for the dead
	// pane there is no live evidence at all: current_path expands empty by
	// tmux design, leaving pane_start_path — the echo of the request — as
	// the only voice, which must never approve placement on its own: a dir
	// that was stat-able but not traversable falls back to the server cwd
	// while retaining the requested start_path (#5174 review).
	//
	// pane_dead is a THREE-valued answer, not a bool: "0" is the affirmative
	// proof that the pane's root process is live, "1" is tmux's dead mark,
	// and anything else — a tmux too old to know the field, a denied or
	// failed expansion, an unanswered format — is UNVERIFIED. Only the
	// affirmative "0" makes pane_pid safe to consult and lets the start-path
	// echo confirm: a pane that MIGHT be dead keeps a retained pid the
	// kernel is free to have recycled onto an unrelated process, and "the
	// field did not say 1" cannot rule that state out (#5174 review).
	deadField, timedOut := t.paneFormatField("#{pane_dead}")
	if timedOut {
		return false, false, false, "", ""
	}
	paneLive := deadField == "0"
	paneDead := deadField == "1"
	sources := []struct {
		name string
		read func() (string, bool)
		// convict is whether a resolved outside-the-worktree answer may
		// produce the negative verdict — and, for a dead pane, whether an
		// inside verdict may stand at all. pane_start_path is denied both:
		// tmux stores the -c it was HANDED (spawn.c keeps the request, not
		// the realized chdir), so its answer names intent, not placement.
		// If the admitted directory was renamed-and-replaced — or a symlink
		// on the path was retargeted — after tmux entered the original
		// inode, the stored path resolves to the replacement while the pane
		// remains correctly bound to the original. The echo can confirm a
		// live pane's match but can never convict (#5174 review).
		convict bool
	}{
		{"proc-cwd", func() (string, bool) { return t.paneProcCwd(paneLive) }, true},
		{"pane_current_path", func() (string, bool) { return t.paneFormatField("#{pane_current_path}") }, true},
		{"pane_start_path", func() (string, bool) { return t.paneFormatField("#{pane_start_path}") }, false},
	}
	walkTimedOut := false
	for _, s := range sources {
		actual, timedOut := s.read()
		if timedOut {
			walkTimedOut = true
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
		if s.name == "proc-cwd" {
			// The procfs answer names whatever process held the retained
			// pid when it was read — the pane's own root ONLY if tmux still
			// marks the pane live. The "0" sampled before the walk can be
			// stale already: a short remain-on-exit command can exit between
			// the opening probe and this read, and once tmux reaps the root
			// the kernel is free to recycle the pid — this answer would then
			// be a stranger's cwd, convicting (or approving) a correctly
			// spawned completed pane (#5174 review). Revalidate AFTER the
			// procfs inspection and discard the source unless the pane is
			// still explicitly live; a fresh dead mark reaches the dead arm
			// below, which decides on evidence the spawn itself authored.
			freshLive, freshDead := t.paneLiveOrDeadNow()
			paneLive = freshLive
			paneDead = paneDead || freshDead
			if !freshLive {
				continue
			}
		}
		if in && !s.convict {
			// A confirm-only answer may stand on live proof, but the bit
			// sampled before the walk can already be stale: a short
			// remain-on-exit command that exits mid-walk leaves paneLive
			// true while procfs and pane_current_path go silent, and the
			// request's own echo must never approve placement on its own
			// (#5174 review). Revalidate before it confirms — and let a
			// fresh dead mark reach the caller's dead arm, which may then
			// convict on evidence the echo could not see.
			freshLive, freshDead := t.paneLiveOrDeadNow()
			paneLive = freshLive
			paneDead = paneDead || freshDead
		}
		switch {
		case in && (s.convict || paneLive):
			return true, true, paneDead, s.name, actual
		case resolvable && s.convict:
			return false, true, paneDead, s.name, actual
		}
		// Inconclusive — an observation that cannot be resolved to a
		// walkable ancestry (a procfs cwd deleted after the spawn: stat on
		// the link answers, EvalSymlinks cannot) whose own inode is not the
		// admitted one, or an answer from a source that may not convict, or
		// a confirm-only answer for a pane that is not proven live. It cannot
		// prove inside OR outside, so it falls through to the next source
		// rather than convicting or clearing.
	}
	// The stale bit cuts the other way too: a pane can die while the sources
	// are read, producing exactly this no-source shape (procfs gone,
	// current_path empty) under a dead flag sampled before it happened.
	// Revalidate once so the caller's dead arm decides on the FRESH mark
	// instead of accepting on silence (#5174 review). A walk that ended on
	// a server timeout does not stack a second deadline on the same silence.
	if !paneDead && !walkTimedOut {
		_, freshDead := t.paneLiveOrDeadNow()
		paneDead = freshDead
	}
	return false, false, paneDead, "", ""
}

// paneLiveOrDeadNow re-reads #{pane_dead} once — the tri-state verdict a
// caller is about to rely on, refreshed past whatever was cached earlier in
// the walk. A short remain-on-exit command can exit between the opening
// probe and the moment its evidence is consumed (#5174 review): "0" proves
// live, "1" proves dead, and a timeout or non-answer proves neither.
func (t *TmuxSession) paneLiveOrDeadNow() (live, dead bool) {
	field, timedOut := t.paneFormatField("#{pane_dead}")
	if timedOut {
		return false, false
	}
	return field == "0", field == "1"
}

// paneShowsSpawnDirRefusal reads the dead pane's scrollback for the launch
// shim's refusal line — the fallback verdict for tmux builds whose
// pane_dead_status expands empty, and for a pane killed by a signal (tmux
// leaves the field empty there too, so the marker is what separates "the
// shim refused to enter" from "the command ran and was signaled"). The pane
// this session's only window holds is the one remain-on-exit retained, so
// the session target reads it directly. The second result reports whether
// the scrollback could be read at all: a failed capture answers (false,
// false) — evidence that never arrived, which the caller treats as
// inconclusive rather than as "no refusal" (#5174 review).
func (t *TmuxSession) paneShowsSpawnDirRefusal() (refused, answered bool) {
	ctx, cancel := tmuxTimeoutContext()
	out, err := t.outputTmuxBounded(ctx, "capture-pane", "-p", "-t", exactTarget(t.sanitizedName))
	cancel()
	if err != nil {
		return false, false
	}
	return strings.Contains(string(out), sessionenv.SpawnDirRefusalMarker), true
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
	// A component the walk cannot stat is one it cannot rule OUT: the
	// observed path or an ancestor may have been renamed or unlinked between
	// EvalSymlinks and this stat, and the failed level might be the very
	// directory that is `want`. Reaching the root only after skipping a
	// failed stat is "cannot tell", not "outside" — convicting on it would
	// tear down a pane that still holds the admitted inode (#5174 review).
	walkFailed := false
	for d := resolved; ; d = filepath.Dir(d) {
		if info, err := statSpawnDir(d); err == nil {
			if os.SameFile(info, want) {
				return true, true
			}
		} else {
			walkFailed = true
		}
		if parent := filepath.Dir(d); parent == d {
			return false, !walkFailed
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
// off Linux, when the pane pid is unknown, or when the pane's live state was
// not affirmatively proven (the pane_dead answer observedPanePlacement fetched
// once for the whole walk). The pid is only safe to read under a live proof —
// pane_dead == "0": a dead pane's retained pid is free to have been recycled
// onto an unrelated process, and a pane_dead query that FAILED to answer —
// unsupported field, denied expansion — leaves exactly the same doubt, so
// "not marked dead" can never license the read (#5174 review). Stat-ing the
// path resolves procfs's symlink to the directory's identity — the kernel's
// record of where the process actually runs, not the -c it was asked for — so
// it still proves the truth even when the worktree itself was unlinked after
// the spawn. The second result propagates paneFormatField's server-silence
// signal.
func (t *TmuxSession) paneProcCwd(paneLive bool) (string, bool) {
	if runtime.GOOS != "linux" || !paneLive {
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
// A blind teardown — the session vanished with no pane ever observed, so the
// descendant sweep ran on the AF_SESSION marker alone — is conclusive about
// the pane but not about what it may have detached: on a markerless session
// (tmux before the per-session environment marker) that sweep is vacuous, so
// the cleanup-authorizing sentinel stays off while the misplaced-pane verdict
// still reports (#5174 review).
func (t *TmuxSession) resolveSpawnedPaneDir(classErr error) error {
	state, blind, closeErr := t.CloseAndWaitForPaneExitReportingBlindness()
	if state == PaneStateKnown && closeErr == nil && !blind {
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
