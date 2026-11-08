package app

import (
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
)

// This file is the TUI's half of the skipped-repos incompleteness channel the
// daemon's Snapshot RPC carries (#4742). The daemon drops a repo at startup when
// its instances.json is corrupted, unreadable, written by a newer schema, or has
// rows that fail to load, and reports the drop in SnapshotResponse.SkippedRepos
// so a client never mistakes the truncated list for a complete one. The CLI's
// list/get/whoami and the web surface the skip set (#5413); the TUI — which the
// field's own documentation names as the client that "mirrors this projection" —
// silently dropped it across all three read paths until this file.
//
// The repair hints below mirror api/skipped_repos.go verbatim so the TUI, the
// CLI and the web send the operator to the SAME remedy. The app package does
// not import api (the TUI is a thin HTTP client with no CLI surface), so the
// hints are duplicated here exactly as the web duplicates them in
// web/src/ui.ts — drift is the bug, and a test asserts the verbatim mirror.

// corruptedRepoRepairHint is the one-line remedy for a repo whose instances.json
// failed to parse at daemon startup (#603). af keeps no backup of these files, so
// the operator has to repair the file themselves. Mirrors api/api.go's
// corruptedRepoRepairHint verbatim.
const corruptedRepoRepairHint = "Stop the af daemon, copy the corrupt file(s) aside, fix the JSON or restore it from a backup, then start the daemon; af keeps no backup of these files."

// unreadableRepoRepairHint is the remedy for a repo the daemon skipped because
// it could not READ its instances.json (#4783). Mirrors api/skipped_repos.go's
// unreadableRepoRepairHint verbatim.
const unreadableRepoRepairHint = "Make the file(s) readable by the user the af daemon runs as (check ownership, permissions and the disk), then restart the daemon so it re-reads them; do not delete them, they hold the hidden sessions."

// newerSchemaRepoRepairHint is the remedy for a repo whose instances.json a
// newer af wrote (#4783). Mirrors api/skipped_repos.go's newerSchemaRepoRepairHint
// verbatim.
const newerSchemaRepoRepairHint = "Upgrade af to a version that understands the newer state file, then restart the daemon; do not edit or delete these files, they hold sessions this binary cannot read."

// rowsFailedRepoRepairHint is the remedy for a repo the daemon skips because its
// instances.json parsed but some of its rows could not be loaded (#4876). Mirrors
// api/skipped_repos.go's rowsFailedRepoRepairHint verbatim.
const rowsFailedRepoRepairHint = "Restore or delete the sessions the daemon could not load (their worktree or tmux is gone), then the daemon re-reads the complete list on the next poll; e.g. `af sessions restore <title>` or `af sessions kill <title>`; the instances.json itself is not the problem."

// skippedRepoForRepoID returns the skip entry for repoID from the skip set, or
// nil if the repo is not skipped. The daemon scopes SkippedRepos to the
// request's RepoID, so a repo-scoped snapshot's skip set names at most the
// current repo (#4742).
func skippedRepoForRepoID(skipped []daemon.SkippedRepo, repoID string) *daemon.SkippedRepo {
	for i := range skipped {
		if skipped[i].RepoID == repoID {
			return &skipped[i]
		}
	}
	return nil
}

// skippedRepoReasonLabel is a short, human-readable label for a skipped repo's
// reason, mirroring the classification in api/skipped_repos.go (splitSkippedRepos)
// and web/src/ui.ts (skippedRepoReasonLabel) so all three surfaces name the same
// failure. Unknown/empty → corrupted (the older-daemon default).
func skippedRepoReasonLabel(reason string) string {
	switch reason {
	case daemon.SkippedRepoReasonUnreadableInstancesJSON:
		return "the daemon could not read its instances.json"
	case daemon.SkippedRepoReasonNewerSchemaInstancesJSON:
		return "its instances.json was written by a newer af"
	case daemon.SkippedRepoReasonRowsFailedToLoad:
		return "some sessions could not be loaded (worktree or tmux gone)"
	default:
		return "its instances.json is corrupted"
	}
}

// skippedRepoRepairHintForReason is the one-line repair hint for a skipped repo's
// reason, mirroring api/skipped_repos.go verbatim so the TUI sends the operator
// to the SAME remedy the CLI does. Unknown/empty → corrupted.
func skippedRepoRepairHintForReason(reason string) string {
	switch reason {
	case daemon.SkippedRepoReasonUnreadableInstancesJSON:
		return unreadableRepoRepairHint
	case daemon.SkippedRepoReasonNewerSchemaInstancesJSON:
		return newerSchemaRepoRepairHint
	case daemon.SkippedRepoReasonRowsFailedToLoad:
		return rowsFailedRepoRepairHint
	default:
		return corruptedRepoRepairHint
	}
}

// skippedRepoStandingNotice is the persistent one-line condition the TUI paints on
// the status bar while the current repo is skipped, so an operator can tell
// "no sessions" from "sessions are hidden" without a transient notice that
// fades. It names the repo and the reason; the full repair hint rides in the
// retained notice (skippedRepoNotice) so `E details` can open it.
func skippedRepoStandingNotice(s *daemon.SkippedRepo) string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("Sessions in this project are hidden — %s. Press E for repair steps.",
		skippedRepoReasonLabel(s.Reason))
}

// skippedRepoNotice is the full repair notice for a skipped repo, carried in the
// errBox's retained slot so `E details` can open it. It names the repo, the
// reason, and the reason-specific repair hint mirroring api/skipped_repos.go.
// Fired as a transient notice when the skip state changes so retained is set;
// the standing notice takes over after the transient fades.
func skippedRepoNotice(s *daemon.SkippedRepo) error {
	if s == nil {
		return nil
	}
	return fmt.Errorf("Sessions in project %q are hidden — %s.\n%s",
		s.RepoID, skippedRepoReasonLabel(s.Reason), skippedRepoRepairHintForReason(s.Reason))
}

// skippedRepoNoticeChanged reports whether two skip entries differ in a way that
// should re-fire the transient notice: a nil-to-non-nil transition, a reason
// change, or a FailedRows change. A same-entry poll does not re-fire.
func skippedRepoNoticeChanged(prev, cur *daemon.SkippedRepo) bool {
	if prev == nil && cur == nil {
		return false
	}
	if prev == nil || cur == nil {
		return true
	}
	return prev.Reason != cur.Reason || prev.FailedRows != cur.FailedRows
}

// consumeSkippedRepoNotice drains a skip-state transition's pending repair-hint
// notice as a tea.Cmd so it lands on the bar and in the errBox's retained slot
// (which `E details` reads). The standing notice already names the condition on
// the bar; this sets the retained slot with the full reason-specific repair hint.
// Returns nil when no notice is pending, so the caller appends it unconditionally.
// The pending text is cleared regardless: a caller that cannot show it (recovery
// screen up) does not re-queue it forever.
func (m *home) consumeSkippedRepoNotice() tea.Cmd {
	if m.pendingSkippedRepoNotice == "" {
		return nil
	}
	notice := m.pendingSkippedRepoNotice
	m.pendingSkippedRepoNotice = ""
	id := m.setTransientNotice(errors.New(notice))
	return m.clearTransientMessageAfterDelay(id)
}
