package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

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
//     unusable — absent, empty, or not a directory — or the spawned pane
//     provably landed elsewhere. Every restore/respawn route maps this to the
//     lost row with the WORKTREE_MISSING reason.
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

// spawnedPaneDirUnusableLogged logs the post-spawn check's skip once per
// process — an environment where no source answers would otherwise repeat
// it on every spawn.
var spawnedPaneDirUnusableLogged sync.Once

// verifySpawnedPaneDir asks tmux where the freshly spawned pane is running and
// tears the session down on a POSITIVE mismatch with the requested directory.
// It runs after the existence poll, so the session this Start created is
// known-present: "the session exists" is not proof the pane started in workDir.
//
// Information that is merely unavailable never tears anything down — a tmux
// that cannot say where the pane is or an answer that does not resolve is
// skipped in favour of the next source, and if every source is silent the
// check is skipped with a one-time INFO log. Sources, in order:
//
//   - /proc/<#{pane_pid}>/cwd: the pane root's live cwd — the kernel's own
//     record of where the process runs, readable through procfs even after
//     the directory itself was unlinked (Linux). The strongest source:
//     it cannot echo the request and cannot be rewritten by tmux.
//   - #{pane_current_path}: where the pane is now; every supported tmux
//     answers it, at the price of an early-chdir program moving the answer.
//   - #{pane_start_path}: weakest, kept last. On tmux >= 3.4 it records the
//     -c tmux was HANDED, not where the pane landed — the #5174 play-test
//     showed it echoing the requested worktree while the process actually
//     ran in the fallback cwd — so it can only confirm the request, never
//     contradict it. On tmux < 3.4 it expands empty.
//
// ErrSessionNotStarted joins a returned error only when teardown of the
// misplaced pane is conclusive, the same contract the readiness-timeout
// path keeps.
func (t *TmuxSession) verifySpawnedPaneDir(workDir string) error {
	wantInfo, wantErr := statSpawnDir(workDir)
	switch {
	case wantErr == nil && wantInfo.IsDir():
		// A comparison is possible; proceed to the sources.
	case wantErr != nil && errors.Is(wantErr, os.ErrNotExist):
		// The directory checkSpawnDir admitted moments ago is gone — the
		// pane necessarily started in tmux's fallback, wherever that is.
		// Proven wrong, not unproven.
		return t.resolveSpawnedPaneDir(fmt.Errorf(
			"%w: start directory %s vanished during spawn: %w",
			ErrSpawnDirMissing, workDir, wantErr))
	case wantErr == nil:
		// A non-directory at the worktree path is not a working directory.
		return t.resolveSpawnedPaneDir(fmt.Errorf(
			"%w: start directory %s is no longer a directory",
			ErrSpawnDirMissing, workDir))
	default:
		// The requested directory's own state is unverifiable — no
		// comparison is possible, and unavailability is never a reason to
		// tear down.
		spawnedPaneDirUnusableLogged.Do(func() {
			log.InfoLog.Printf("session %s: post-spawn cwd check skipped: cannot stat requested start directory %s: %v",
				t.sanitizedName, workDir, wantErr)
		})
		return nil
	}

	sources := []struct {
		name string
		read func() string
	}{
		{"proc-cwd", t.paneProcCwd},
		{"pane_current_path", func() string { return t.paneFormatField("#{pane_current_path}") }},
		{"pane_start_path", func() string { return t.paneFormatField("#{pane_start_path}") }},
	}
	for _, source := range sources {
		actual := source.read()
		if !filepath.IsAbs(actual) {
			// Empty (unsupported on this tmux) or unusable — try the next
			// source rather than treat silence as a mismatch.
			continue
		}
		gotInfo, err := statSpawnDir(actual)
		if err != nil {
			continue
		}
		if os.SameFile(gotInfo, wantInfo) {
			return nil
		}
		return t.resolveSpawnedPaneDir(fmt.Errorf(
			"%w: pane for session %s started in %s, not the requested %s (via %s)",
			ErrSpawnDirMissing, t.sanitizedName, actual, workDir, source.name))
	}

	spawnedPaneDirUnusableLogged.Do(func() {
		log.InfoLog.Printf("session %s: post-spawn cwd check skipped: no tmux source could report the spawned pane's working directory",
			t.sanitizedName)
	})
	return nil
}

// paneFormatField answers one tmux display-message format for this session,
// or "" when the query fails or the field expands to nothing — which on a
// tmux that does not know the field is exactly how "unsupported" arrives.
func (t *TmuxSession) paneFormatField(format string) string {
	ctx, cancel := tmuxTimeoutContext()
	out, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t",
		exactTarget(t.sanitizedName), format)
	cancel()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// procfsRoot is the procfs mount point — a var so tests can point it at a
// fixture instead of the host's real process table.
var procfsRoot = "/proc"

// paneProcCwd returns /proc/<pane_pid>/cwd for the freshly spawned pane, or ""
// off Linux or when the pane pid is unknown. Stat-ing the path resolves
// procfs's symlink to the directory's identity — the kernel's record of where
// the process actually runs, not the -c it was asked for — so it still proves
// the truth even when the worktree itself was unlinked after the spawn.
func (t *TmuxSession) paneProcCwd() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	pid, err := strconv.Atoi(t.paneFormatField("#{pane_pid}"))
	if err != nil || pid <= 0 {
		return ""
	}
	return filepath.Join(procfsRoot, strconv.Itoa(pid), "cwd")
}

// resolveSpawnedPaneDir tears down the just-created session whose pane
// provably did not start in the requested directory — a pane af cannot place
// there never survives under a row that will report ready.
func (t *TmuxSession) resolveSpawnedPaneDir(classErr error) error {
	state, closeErr := t.Close()
	if state == PaneStateKnown && closeErr == nil {
		return fmt.Errorf("%w: %w", ErrSessionNotStarted, classErr)
	}
	if closeErr != nil {
		return fmt.Errorf("%v (cleanup error: %v)", classErr, closeErr)
	}
	return classErr
}
