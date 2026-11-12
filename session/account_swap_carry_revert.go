package session

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// A Codex backtrack (Esc-Esc in the TUI) calls thread/revert. The thread keeps
// its id and continues in a new immutable rollout,
// rollout-<ts>-<thread>_<rollout>.jsonl, whose session_meta history_base points
// into a prefix of the rollout before it. The original stays on disk, frozen at
// the revert (codex-rs thread-store local/revert_thread.rs).
//
// Which of those files `codex resume <thread>` opens is decided by Codex's own
// state database: once it knows the thread, its rollout-path pointer is
// authoritative, and only a thread it has no row for falls back to the newest
// matching filename (thread_rollout_resolver.rs, rollout list.rs
// find_thread_path_by_id_str). af neither reads nor writes that database, so it
// cannot name the rollout the source account would resume, nor make the new
// account resume the same one. A carry of a backtracked thread therefore
// starts fresh with a stated reason, instead of copying the plain original and
// resuming the turns the user backtracked away from (#7141).

const (
	// codexRevertCarrySourceReason: the outgoing account's Codex continues the
	// thread in a revert rollout, so the plain original af would copy is the
	// conversation from before the backtrack.
	codexRevertCarrySourceReason = "it was backtracked in Codex and continues in a thread/revert rollout, which af does not carry yet"
	// codexRevertCarryTargetReason: the incoming account's Codex would resolve
	// the thread to a revert rollout it already holds, not to the copy.
	codexRevertCarryTargetReason   = "the new account already holds a thread/revert rollout of this conversation, which Codex would resume instead of the carried one"
	codexRevertCarryUnlistedReason = "af could not enumerate the Codex rollout store"
)

// codexRolloutIsRevert reports whether path names a thread/revert rollout,
// compressed or not.
func codexRolloutIsRevert(path string) bool {
	m := codexRolloutFormRE.FindStringSubmatch(filepath.Base(path))
	return len(m) == 4 && m[2] != ""
}

// codexHoldsRevertRollout reports whether codexHome's store files thread id
// under any thread/revert rollout dated on or after floor's day (zero: any).
// A store that cannot be walked fails closed: the walk stops at its first
// failure, so what it found proves nothing.
func codexHoldsRevertRollout(codexHome, id string, floor time.Time) (bool, error) {
	_, other, err := codexRolloutFormsSince(codexHome, id, floor)
	if err != nil {
		return false, err
	}
	for _, path := range other {
		if codexRolloutIsRevert(path) {
			return true, nil
		}
	}
	return false, nil
}

// codexRevertReason is why this carry cannot take its thread because of a
// thread/revert rollout, or "". It runs when the carry is planned, before the
// outgoing runtime stops, and again just before the copy, after it has: a
// backtrack in between must stop the carry too.
//
// The source is always checked. The destination is checked only until the
// carry commits: after that the replacement has run in the new account, and a
// revert rollout there is that account's own backtrack, which its Codex
// resolves through its own state.
func (c *conversationCarry) codexRevertReason() string {
	if c.agent != tmux.ProgramCodex {
		return ""
	}
	floor := codexRevertFloor(c.transcript)
	held, err := codexHoldsRevertRollout(c.srcHome, c.id, floor)
	switch {
	case err != nil:
		return codexRevertCarryUnlistedReason
	case held:
		return codexRevertCarrySourceReason
	case c.committed:
		return ""
	}
	held, err = codexHoldsRevertRollout(c.dstHome, c.id, floor)
	switch {
	case err != nil:
		return codexRevertCarryUnlistedReason
	case held:
		return codexRevertCarryTargetReason
	}
	return ""
}

// codexRevertFloor bounds where a thread/revert rollout of the thread whose
// plain rollout is filed at rel can be. A revert is minted after the rollout
// it reverts, and both are filed under the recorder's local calendar date, so
// nothing earlier than the plain rollout's day — less the lookback that covers
// a clock or time-zone change between the two — can hold one (#7152). The
// revert check runs under the swap's global fences, and a long-lived shared
// store would otherwise be walked whole, twice per check. A rel that is not a
// sessions/YYYY/MM/DD path gives the zero floor: the whole store.
func codexRevertFloor(rel string) time.Time {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 5 || parts[0] != "sessions" {
		return time.Time{}
	}
	day, err := time.ParseInLocation("2006/01/02", strings.Join(parts[1:4], "/"), time.Local)
	if err != nil {
		return time.Time{}
	}
	return codexRolloutScanFloor(day)
}
