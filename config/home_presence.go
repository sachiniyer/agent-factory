package config

import (
	"os"

	"github.com/sachiniyer/agent-factory/internal/afhome"
)

// The config-package face of the abandoned-daemon write-side latch: nothing this
// process writes may re-create an AF home it has already observed present and
// then seen deleted (#3842/#3845/#3850).
//
// The mechanism lives in internal/afhome, and its package comment carries the
// reasoning — why a latch rather than "the home must exist", why only a positive
// ENOENT refuses, and why arming is fail-open. It moved down there in #3850
// because config imports both log and session/tmux, so neither can import config
// back, and both create directories that ARE the AF home or sit inside it.
//
// These are exact aliases, not a second policy. Packages that already depend on
// config keep this spelling; packages below config call afhome directly.

// ErrAFHomeRemoved reports that a write or directory creation was refused
// because the AF home this process observed at startup has since been deleted.
// Callers match on it with errors.Is to tell this deliberate refusal from an I/O
// failure.
var ErrAFHomeRemoved = afhome.ErrRemoved

// MarkAFHomePresent latches home as observed-present for this process and
// returns the release. It is the daemon-startup arming; nothing else calls it.
func MarkAFHomePresent(home string) (func(), error) { return afhome.MarkPresent(home) }

// MkdirAllUnderAFHome is os.MkdirAll for a directory that can be at or inside
// the AF home. It is an exact os.MkdirAll when the latch is unarmed (every CLI
// process) or when the home is present.
func MkdirAllUnderAFHome(dir string, perm os.FileMode) error { return afhome.MkdirAll(dir, perm) }

// requireObservedAFHomePresent refuses when path is at or inside a home this
// process observed present and that is now positively gone. It is the
// precondition at the head of ensureStorageParent.
func requireObservedAFHomePresent(path string) error { return afhome.RequirePresent(path) }
