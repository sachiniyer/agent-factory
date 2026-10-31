package config

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// InRepoUnknownLeaf is one leaf under an allowed structured config table that
// no typed field matches: an in-repo [docker] or [ssh] table (no
// DockerConfig/SSHConfig field) or a personal per-project [root_agent] table
// (no RootAgent field). The typed decode would drop it, so the setting it was
// meant to carry would have no effect. The in-repo loader REJECTS such a file
// with an InRepoUnknownLeavesError (#4599 warned first; #4845 made it an error);
// the personal loader still reports [root_agent] leaves as a warning.
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

// InRepoUnknownLeavesError is the load error for one or more unknown [docker]/[ssh]
// leaves in an in-repo config (#4845). Each leaf's Message names the file, the key
// and, when a known key is close, the intended one; Error joins them one per line.
// af doctor reads Leaves to report each as its own finding.
type InRepoUnknownLeavesError struct {
	// Path is the config file, in the home-abbreviated form load errors use.
	Path   string
	Leaves []InRepoUnknownLeaf
}

func (e *InRepoUnknownLeavesError) Error() string {
	lines := make([]string, 0, len(e.Leaves))
	for _, leaf := range e.Leaves {
		lines = append(lines, leaf.Message)
	}
	return strings.Join(lines, "\n")
}

// rejectedInRepoLeaf builds the report for one unknown leaf that fails the load:
// the file, the table and key, the closest valid key ("did you mean …?") when one
// is near, and the allowed set so the fix needs no docs.
func rejectedInRepoLeaf(prettyPath, table, leaf string, allowed map[string]bool) InRepoUnknownLeaf {
	suggestion := closestTableLeafSuggestion(strings.ToLower(leaf), allowed)
	didYouMean := ""
	if suggestion != "" {
		didYouMean = fmt.Sprintf(" did you mean %q?", suggestion)
	}
	return InRepoUnknownLeaf{
		Path:       prettyPath,
		Table:      table,
		Key:        leaf,
		Suggestion: suggestion,
		Message: fmt.Sprintf("in-repo config %s: unknown key %q under %q;%s fix or remove it (allowed %s keys: %s)",
			prettyPath, leaf, table, didYouMean, table, strings.Join(sortedKeysFrom(allowed), ", ")),
	}
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
