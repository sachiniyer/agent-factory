package sessionenv

import (
	"path/filepath"

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

// SpawnDirPrefix returns the leading NAME=value word that hands the pane's
// exec shim the directory it must run in, or "" when dir cannot be pinned to
// an absolute path. The shim's own os.Chdir is the last place misplacement
// can still be prevented — it repairs tmux's silent -c fallback or refuses
// outright — so the assignment is resolved by the DAEMON's cwd (filepath.Abs),
// never the pane's, before it is quoted into the launch line.
func SpawnDirPrefix(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil || abs == "" {
		return ""
	}
	return SpawnDirEnv + "=" + shellquote.Quote(abs) + " "
}
