package config

import (
	"fmt"
	"io"
	"sort"
	"sync"
)

// InRepoUnknownLeaf is one leaf under an in-repo [docker] or [ssh] table that
// no DockerConfig/SSHConfig field matches. The typed decode drops it, so the
// setting it was meant to carry has no effect (#4599).
type InRepoUnknownLeaf struct {
	// Path is the config file, in the home-abbreviated form load errors use.
	Path string
	// Table is "docker" or "ssh".
	Table string
	// Key is the leaf exactly as spelled in the file.
	Key string
	// Suggestion is the closest known leaf of Table, or "" when none is near.
	Suggestion string
	// Message is the warning line the log, CLI stderr, and af doctor show.
	Message string
}

// UnknownLeaves reports the [docker]/[ssh] leaves the load dropped because no
// field matches them, sorted by table then key. Empty for a clean file. Unlike
// the warning, which is emitted once per process, this is reported on every
// load, so af doctor sees it even after the CLI already warned.
func (c *InRepoConfig) UnknownLeaves() []InRepoUnknownLeaf {
	if c == nil {
		return nil
	}
	return append([]InRepoUnknownLeaf(nil), c.unknownLeaves...)
}

func sortInRepoUnknownLeaves(leaves []InRepoUnknownLeaf) []InRepoUnknownLeaf {
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].Table != leaves[j].Table {
			return leaves[i].Table < leaves[j].Table
		}
		return leaves[i].Key < leaves[j].Key
	})
	return leaves
}

var (
	interactiveWarningsMu sync.Mutex
	// interactiveWarnings receives config warnings a person running af should
	// see directly. log.WarningLog alone does not do that for a CLI command: it
	// writes to the log file. nil (the default) means log-only, which is what
	// the daemon, and the TUI once it owns the terminal, must be.
	interactiveWarnings io.Writer
)

// SetInteractiveWarningWriter routes config warnings that need a person's
// attention to w (os.Stderr for an interactive CLI command) in addition to the
// log. Pass nil to make them log-only again.
func SetInteractiveWarningWriter(w io.Writer) {
	interactiveWarningsMu.Lock()
	defer interactiveWarningsMu.Unlock()
	interactiveWarnings = w
}

func writeInteractiveWarning(msg string) {
	interactiveWarningsMu.Lock()
	defer interactiveWarningsMu.Unlock()
	if interactiveWarnings != nil {
		fmt.Fprintln(interactiveWarnings, "warning: "+msg)
	}
}
