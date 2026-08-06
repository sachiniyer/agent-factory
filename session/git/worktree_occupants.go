package git

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// Occupant is a process positively observed to be operating inside a worktree.
//
// "Positively observed" is the whole contract. The only way into this list is a
// readable cwd that resolves at-or-under the worktree root — a fact about what
// the process IS DOING, not an inference from something it is missing. Nothing
// is ever added because it lacks a marker (#2998).
type Occupant struct {
	Process proctree.Process
	// WorkingDir is the resolved cwd that matched, kept so a report can say WHY
	// this process was attributed rather than asking the operator to trust it.
	WorkingDir string
}

// WorktreeOccupants reports every process whose working directory is inside
// worktreePath, and never kills anything.
//
// # Why this exists
//
// af attributes a session's descendants by the AF_SESSION marker its pane
// exports. That marker is not always there: `sessionEnvFlags` returns nil when
// tmux is older than 3.2 (no `new-session -e`), and sessions created by
// pre-marker builds never had one. For those, a marker scan finds nothing
// because nothing was ever MARKED — which is indistinguishable from finding
// nothing because nothing survived. A descendant that escaped a lost-ancestry
// teardown is then invisible, and cleanup proceeds over it (#2998).
//
// The cwd is a marker-independent signal that does not depend on how the
// session was created, how old the binary was, or what tmux supports.
//
// # What a match does and does not prove
//
// It proves OCCUPANCY: this process is working inside a directory af created and
// owns. It does NOT prove af started it — an operator's own shell sitting in the
// worktree matches identically, and no readable property distinguishes the two.
//
// So this function only ever REPORTS. Deciding an occupant is af's and may be
// signalled is a separate judgement with a much higher bar, and the caller that
// makes it (reapWorktreeWriters, at removal time) owns that decision explicitly.
// Attribution and authorisation are kept apart here on purpose: conflating them
// is how "it looked like ours" becomes "so we killed it".
//
// # Unknown is an error, never an empty list
//
// A process table that cannot be read returns an error. It must never be
// collapsed into "no occupants" — that is the exact substitution that let an
// unreadable list read as "everything is dead" and armed a sweep against live
// sessions (#2874). A process whose individual cwd is unreadable is skipped
// rather than guessed at, which can only omit a real occupant, never invent one.
func WorktreeOccupants(worktreePath string) ([]Occupant, error) {
	if worktreePath == "" {
		return nil, fmt.Errorf("cannot look for worktree occupants: no worktree path was given")
	}
	root := normalizeWorktreePath(worktreePath)
	snap, err := proctree.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("cannot read the process table to look for processes inside %s: %w", root, err)
	}

	seen := make(map[int]bool)
	var occupants []Occupant
	for pid := range snap {
		cwd, ok := proctree.WorkingDir(pid)
		if !ok {
			// The kernel will not disclose this one's cwd: a foreign process, a
			// kernel thread, or one that exited in the gap. Unreadable is not a
			// match and is not a miss — it is simply not evidence, and treating it
			// as either would be inventing a fact.
			continue
		}
		if !pathAtOrUnder(root, filepath.Clean(cwd)) {
			continue
		}
		// The whole subtree, because a child of a worktree-cwd'd process belongs to
		// the same occupancy even if it chdir'd elsewhere — and it is frequently the
		// one actually holding files open. Its membership is inherited from a
		// POSITIVE match on its ancestor, not assumed from its own cwd.
		for _, descendant := range proctree.TreeOf(snap, pid) {
			if seen[descendant.PID] {
				continue
			}
			seen[descendant.PID] = true
			// The matched ancestor reports the cwd that matched; a descendant
			// attributed through it reports its own, when readable, so a report can
			// show the chain rather than implying each one matched directly.
			dir := cwd
			if descendant.PID != pid {
				if own, ok := proctree.WorkingDir(descendant.PID); ok {
					dir = own
				}
			}
			occupants = append(occupants, Occupant{Process: descendant, WorkingDir: dir})
		}
	}
	sort.Slice(occupants, func(i, j int) bool { return occupants[i].Process.PID < occupants[j].Process.PID })
	return occupants, nil
}

// DescribeOccupants renders occupants for an operator-facing message. Kept
// beside the detection so every surface that reports occupancy says the same
// thing the same way, including WHY each process was attributed.
func DescribeOccupants(occupants []Occupant) string {
	const maxListed = 5
	out := ""
	for i, o := range occupants {
		if i == maxListed {
			out += fmt.Sprintf(", and %d more", len(occupants)-maxListed)
			break
		}
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("pid %d (cwd %s)", o.Process.PID, o.WorkingDir)
	}
	return out
}
