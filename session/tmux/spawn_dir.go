package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
//     unusable — absent, empty, or not a directory — or the spawned pane
//     provably landed elsewhere. Every restore/respawn route maps this to the
//     lost row with the WORKTREE_MISSING reason.
//   - Unknown (ErrSpawnDirUnknown): the directory's state could not be
//     established — a stat error other than ENOENT, or tmux would not report
//     the pane's start path. Not evidence the worktree is gone: the row is
//     held and retried on a later pass rather than declared missing.

// statSpawnDir is os.Stat renamed for this seam. A var so tests can stage
// non-ENOENT stat failures — permission and I/O errors — that a real
// filesystem cannot produce portably (a root test runner ignores chmod).
var statSpawnDir = os.Stat

// checkSpawnDir refuses a spawn whose start directory is unusable, before any
// tmux command has run. Callers invoke it while the session name is already
// known-absent, so the refusal can carry ErrSessionNotStarted: nothing was
// ever spawned.
func checkSpawnDir(workDir string) error {
	if workDir == "" {
		// An empty -c asks tmux for its default — the server's cwd — which is
		// exactly the fallback this seam exists to forbid.
		return fmt.Errorf("%w: %w: %w: no start directory was requested",
			ErrSessionNotStarted, ErrSpawnDirMissing, os.ErrNotExist)
	}
	info, err := statSpawnDir(workDir)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		// A non-directory at the worktree path is not a working directory;
		// tmux would fall back exactly as if it were absent.
		return fmt.Errorf("%w: %w: %w: %s exists but is not a directory",
			ErrSessionNotStarted, ErrSpawnDirMissing, os.ErrNotExist, workDir)
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: %w: %s: %w",
			ErrSessionNotStarted, ErrSpawnDirMissing, workDir, err)
	default:
		return fmt.Errorf("%w: %w: cannot verify %s: %w",
			ErrSessionNotStarted, ErrSpawnDirUnknown, workDir, err)
	}
}

// verifySpawnedPaneDir reads the freshly spawned pane's recorded start
// directory — #{pane_start_path}, set at spawn and never rewritten, so a
// program that immediately chdirs cannot mask where tmux actually put it —
// and reports whether it resolves to the requested directory. It runs after
// the existence poll, so the session this Start created is known-present:
// "the session exists" is not proof the pane started in workDir.
//
// On any negative answer the just-created session is killed — a pane af cannot
// place in its requested directory never survives under a row that will report
// ready. ErrSessionNotStarted joins the returned error only when the teardown
// is conclusive, the same contract the readiness-timeout path keeps.
func (t *TmuxSession) verifySpawnedPaneDir(workDir string) error {
	ctx, cancel := tmuxTimeoutContext()
	out, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t",
		exactTarget(t.sanitizedName), "#{pane_start_path}")
	cancel()

	var classErr error
	switch {
	case err != nil:
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: display-message pane_start_path after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		classErr = fmt.Errorf("%w: could not read spawned pane's start directory for session %s: %w",
			ErrSpawnDirUnknown, t.sanitizedName, err)
	default:
		actual := strings.TrimSpace(string(out))
		wantInfo, wantErr := statSpawnDir(workDir)
		switch {
		case wantErr != nil && errors.Is(wantErr, os.ErrNotExist):
			// The directory checked moments ago is gone now — the pane
			// necessarily started in tmux's fallback, wherever that is.
			classErr = fmt.Errorf("%w: start directory %s vanished during spawn: %w",
				ErrSpawnDirMissing, workDir, wantErr)
		case wantErr != nil:
			classErr = fmt.Errorf("%w: could not re-check start directory %s: %w",
				ErrSpawnDirUnknown, workDir, wantErr)
		case !filepath.IsAbs(actual):
			// A real tmux always records an absolute start path; an empty or
			// relative answer is not an answer af can verify against.
			classErr = fmt.Errorf("%w: tmux reported no usable start path for session %s (got %q)",
				ErrSpawnDirUnknown, t.sanitizedName, actual)
		default:
			gotInfo, gotErr := statSpawnDir(actual)
			switch {
			case gotErr != nil:
				classErr = fmt.Errorf("%w: recorded pane start path %s does not resolve: %w",
					ErrSpawnDirUnknown, actual, gotErr)
			case !os.SameFile(gotInfo, wantInfo):
				// os.SameFile, not string compare: tmux may record a
				// canonicalized spelling of a symlinked request path.
				classErr = fmt.Errorf("%w: pane for session %s started in %s, not the requested %s",
					ErrSpawnDirMissing, t.sanitizedName, actual, workDir)
			}
		}
	}
	if classErr == nil {
		return nil
	}

	state, closeErr := t.Close()
	if state == PaneStateKnown && closeErr == nil {
		return fmt.Errorf("%w: %w", ErrSessionNotStarted, classErr)
	}
	if closeErr != nil {
		return fmt.Errorf("%v (cleanup error: %v)", classErr, closeErr)
	}
	return classErr
}
