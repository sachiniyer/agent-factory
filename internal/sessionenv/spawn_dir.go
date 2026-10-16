package sessionenv

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

const (
	// SpawnDirEnv names the variable the pane launch line hands the exec
	// shim: the directory the pane's command must run in. It travels as a
	// leading NAME=value assignment inside the wrapped command itself — a
	// channel every supported tmux carries, unlike `new-session -e` (3.2+) —
	// and the agent never sees it: the name sits outside every allowlist the
	// exec filter builds, so it is stripped from the child environment
	// alongside the other af-owned markers.
	SpawnDirEnv = "AF_SPAWN_DIR"
	// SpawnDirInodeEnv names the second variable of the pair: the admitted
	// directory's device:inode identity, captured when checkSpawnDir admitted
	// it. A pathname is only a request — between the daemon's admission and
	// the pane's chdir, a rename-and-replace or a retargeted worktree symlink
	// can make the same path resolve to a directory af never admitted, and
	// the shim would exec the pane command inside it. Comparing the entered
	// inode against this value closes that window at the only point that can
	// still prevent the wrong-directory exec (#5174 review). Like SpawnDirEnv
	// the agent never sees it.
	SpawnDirInodeEnv = "AF_SPAWN_DIR_INO"
	// SpawnDirRefusalExitCode is the shim's exit status when it cannot chdir
	// into the SpawnDirEnv directory — the one placement verdict authored at
	// the spawn instant itself. tmux records it in pane_dead_status, so a
	// pane that exits before any live source can answer still PROVES it never
	// ran inside the requested directory, where a point-in-time filesystem
	// re-check could only describe the verification instant (#5174 review).
	SpawnDirRefusalExitCode = 113
	// SpawnDirRefusalMarker prefixes the stderr line the shim leaves in a
	// refused pane's scrollback. It is the same verdict on tmux versions
	// whose pane_dead_status expands empty — and on a signal-killed pane,
	// where tmux leaves the field empty too.
	SpawnDirRefusalMarker = "af: cannot start the session command in its requested directory"
)

// SpawnDirPrefix returns the leading NAME=value words that hand the pane's
// exec shim the directory it must run in AND the admitted inode identity it
// must verify after chdir, or "" when dir cannot be pinned to an absolute
// path. The shim's own os.Chdir is the last place misplacement can still be
// prevented — it repairs tmux's silent -c fallback or refuses outright — so
// the directory assignment is resolved by the DAEMON's cwd (filepath.Abs),
// never the pane's, before it is quoted into the launch line. admitted is the
// FileInfo checkSpawnDir validated: when the platform exposes its inode pair
// the second assignment travels too, letting the shim prove the pathname
// landed on the directory af admitted rather than a replacement that took
// its name (#5174 review).
func SpawnDirPrefix(dir string, admitted os.FileInfo) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil || abs == "" {
		return ""
	}
	prefix := SpawnDirEnv + "=" + shellquote.Quote(abs)
	if dev, ino, ok := spawnDirInodeID(admitted); ok {
		prefix += " " + SpawnDirInodeEnv + "=" +
			strconv.FormatUint(dev, 10) + ":" + strconv.FormatUint(ino, 10)
	}
	return prefix + " "
}

// parseSpawnDirInode decodes the SpawnDirInodeEnv value the launch line
// carried: device:inode as unsigned decimal. A malformed value is reported,
// never guessed at — the verifier refuses rather than skip a corrupted
// identity claim.
func parseSpawnDirInode(value string) (dev, ino uint64, ok bool) {
	devStr, inoStr, found := strings.Cut(value, ":")
	if !found {
		return 0, 0, false
	}
	parsedDev, err := strconv.ParseUint(devStr, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	parsedIno, err := strconv.ParseUint(inoStr, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return parsedDev, parsedIno, true
}
