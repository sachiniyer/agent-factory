package app

import (
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/overlay"
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
// fades. It names the reason; the full repair hint rides in the skip-owned
// retained slot (skippedRepoRetained) so `E details` can open it. The closing
// "Press <key> for repair steps." instruction is built from the same state-aware
// binding noticeDetailsKey uses (passed in as detailsKey), so the standing text
// never advertises a key that does not open the details in the current state —
// a captive Projects section, a rebound error_details, or an overlay that owns
// the keyboard. An empty detailsKey omits the instruction instead of naming a
// dead key (#4742 review).
func skippedRepoStandingNotice(s *daemon.SkippedRepo, detailsKey string) string {
	if s == nil {
		return ""
	}
	if detailsKey == "" {
		return fmt.Sprintf("Sessions in this project are hidden — %s.", skippedRepoReasonLabel(s.Reason))
	}
	return fmt.Sprintf("Sessions in this project are hidden — %s. Press %s for repair steps.",
		skippedRepoReasonLabel(s.Reason), detailsKey)
}

// skippedRepoNotice is the full repair notice for a skipped repo, carried in the
// errBox's retained slot so `E details` can open it. It names the repo, the
// reason, the reason-specific repair hint mirroring api/skipped_repos.go, and —
// for rows-failed-to-load — the FailedRows count the daemon supplies so a
// change from one hidden session to many produces a different notice and the
// operator can see what changed (mirroring the CLI and web, #4742 review).
// Fired as a transient notice when the skip state changes so retained is set;
// the standing notice takes over after the transient fades.
func skippedRepoNotice(s *daemon.SkippedRepo) error {
	if s == nil {
		return nil
	}
	reason := skippedRepoReasonLabel(s.Reason)
	if s.Reason == daemon.SkippedRepoReasonRowsFailedToLoad && s.FailedRows > 0 {
		reason = fmt.Sprintf("%s (%d session(s) hidden)", reason, s.FailedRows)
	}
	return fmt.Errorf("Sessions in project %q are hidden — %s.\n%s",
		s.RepoID, reason, skippedRepoRepairHintForReason(s.Reason))
}

// skippedRepoNoticeChanged reports whether two skip entries differ in a way that
// should re-fire the transient notice: a nil-to-non-nil transition, a RepoID
// change, a reason change, or a FailedRows change. A same-entry poll does not
// re-fire. The RepoID is part of the comparison so switching directly between
// two skipped projects (same reason/FailedRows) still re-fires for the incoming
// project instead of retaining the outgoing one's entry.
func skippedRepoNoticeChanged(prev, cur *daemon.SkippedRepo) bool {
	if prev == nil && cur == nil {
		return false
	}
	if prev == nil || cur == nil {
		return true
	}
	return prev.RepoID != cur.RepoID || prev.Reason != cur.Reason || prev.FailedRows != cur.FailedRows
}

// skippedRepoStandingDetailsKey returns the label of the key that opens the
// skip's repair details in the current state, or "" when no key does — in which
// case skippedRepoStandingNotice omits the "Press … for repair steps."
// instruction rather than advertise a key that does nothing (a captive
// Projects section, a rebound error_details, an overlay that owns the
// keyboard). It is the same state-aware binding noticeDetailsKey uses, so the
// standing text and the bar's details hint always agree on whether `E` works
// (#4742 review).
func (m *home) skippedRepoStandingDetailsKey() string {
	b, ok := m.noticeDetailsKey()
	if !ok {
		return ""
	}
	return keys.HelpLabel(b.Keys())
}

// consumeSkippedRepoNotice drains a skip-state transition's pending repair-hint
// notice as a tea.Cmd so it lands on the bar for one cycle. The standing notice
// already names the condition on the bar; the full repair hint rides in the
// skip-owned skippedRepoRetained slot (which `E details` reads) so a later
// transient cannot overwrite it (#4742 review). The transient copy here is a
// hint, not a notice: it paints on the same bar and fades on the same timer, but
// it never lands in the shared retained slot, so the skip's details stay bound
// to the skip condition and retract with it. Returns nil when no notice is
// pending, so the caller appends it unconditionally. The pending text is
// cleared regardless: a caller that cannot show it (recovery screen up) does
// not re-queue it forever.
//
// A newer transient raised between the queue and the drain (a cold start or
// project switch can queue this before the snapshot handler drains it, and
// another operation may raise an error or notice in that interval) must not be
// overwritten by the older skip guidance: an unconditional SetHint would paint
// the skip hint over the newer message, and showErrorDetails would then route
// E to the skip's hint while the overwritten failure stays marooned in the
// shared retained slot for as long as the repo stays skipped (#4742 review).
// Skip painting when a different transient is already on the bar; the skip's
// standing notice and its condition-owned retained slot still carry the repair
// guidance, so E details and the standing line remain correct.
func (m *home) consumeSkippedRepoNotice() tea.Cmd {
	if m.pendingSkippedRepoNotice == "" {
		return nil
	}
	notice := m.pendingSkippedRepoNotice
	m.pendingSkippedRepoNotice = ""
	skipDetails := m.errBox.SanitizeNoticeText(notice)
	if bar := m.errBox.FullError(); bar != "" && bar != skipDetails {
		// A newer transient is on the bar; leave it alone.
		return nil
	}
	id := m.setTransientHint(errors.New(notice))
	return m.clearTransientMessageAfterDelay(id)
}

// skippedRepoIDSet converts the daemon's startup-skip set (#4742) into the
// RepoID lookup the Projects-section row builder uses to mark a skipped repo's
// zero count as hidden rather than absent.
func skippedRepoIDSet(skipped []daemon.SkippedRepo) map[string]bool {
	if len(skipped) == 0 {
		return nil
	}
	out := make(map[string]bool, len(skipped))
	for _, s := range skipped {
		out[s.RepoID] = true
	}
	return out
}

// skippedRepoHintSet converts the daemon's startup-skip set (#4742) into the
// RepoID → repair-hint map the Projects-section row builder carries so the D
// refusal names each hidden row's applicable remedy directly, mirroring the
// picker's SetSkippedRepoHints (#4742 review).
func skippedRepoHintSet(skipped []daemon.SkippedRepo) map[string]string {
	if len(skipped) == 0 {
		return nil
	}
	out := make(map[string]string, len(skipped))
	for _, s := range skipped {
		out[s.RepoID] = skippedRepoRepairHintForReason(s.Reason)
	}
	return out
}

// updateOpenPickerSkipState propagates a background poll's cross-repo skip set
// and fetch status into a picker that is still open (#4742 review): a repo that
// becomes skipped while Ctrl+P stays open must mark its row hidden and gate the
// D path for it, instead of retaining the skip set captured when the picker
// opened. A failed background fetch sets SnapshotDegraded so the picker's D
// gate falls back to the global fetch-failure refusal, but RETAINS the
// picker's last-known skip IDs and hints: the failed request arrives with a
// nil skip set yet cannot establish that a previously skipped repo was
// repaired, so clearing them would render the row ordinary while the sidebar
// still marks it hidden (#4742 review). Mirrors applySidebarProjects, which
// reuses its retained sidebarProjectsSkipped on the same error path. No-op
// when no picker is open.
//
// A successful fetch also refreshes the picker's ROWS from the same snapshot:
// the D path reads the row counts, so clearing the degraded guard (or a
// repaired repo's skip gate) must be backed by fresh counts. Without this, a
// picker opened during a failed fetch (or while a repo was skipped) keeps
// stale zero counts after a successful poll clears the guard, and the D path
// can build a "no live sessions" confirmation for a project whose sessions the
// successful poll just reported (#4742 review).
func (m *home) updateOpenPickerSkipState(data []session.InstanceData, skipped []daemon.SkippedRepo, fetchErr error) {
	if m.projectPickerOverlay == nil || m.state != stateSwitchProject {
		return
	}
	// RefreshRows is a no-op while the add/rebind path-input form is active,
	// so the rows stay stale for as long as the form is open. Applying the
	// poll's guard/skip-ID changes anyway would clear SnapshotDegraded and
	// replace skip IDs without installing the fresh counts behind them, and
	// the user could then close the form and press D against the stale rows
	// — deletion enabled on counts the form-suppressed refresh never
	// installed. Defer the whole update to the next poll after the form
	// closes (#4742 review).
	if m.projectPickerOverlay.FormActive() {
		return
	}
	m.projectPickerOverlay.SetSnapshotDegraded(fetchErr != nil)
	if fetchErr != nil {
		// Keep the picker's last-known skip IDs/hints (captured at open time
		// or the last successful poll): the failed request cannot establish
		// that a previously skipped repo was repaired, so clearing them
		// would render the row ordinary while the sidebar still marks it
		// hidden. SnapshotDegraded(true) above already gates the D path on
		// the global fetch-failure refusal, so deletion stays disabled
		// (#4742 review).
		return
	}
	projects, registryDegraded := m.buildProjectListFrom(data)
	// A degraded registry read omits registry-only projects from the fresh
	// list; passing it to RefreshRows wholesale would drop them from the
	// open picker until another poll succeeds, taking their switch/rebind
	// actions with them. The Projects section handles the same condition by
	// merging its previous registry-backed rows; mirror that here so Ctrl+P
	// keeps the registered-but-sessionless projects through the degradation
	// (#4742 review).
	if registryDegraded {
		old := registryBackedRows(m.projectPickerOverlay.Projects())
		projects = overlay.MergePreservingSessionRows(old, projects, true)
	}
	// Preserve last-known rows for skipped IDs the fresh snapshot no longer
	// represents (#4742 review): an inactive repo discovered only from its
	// live sessions has no instance once the daemon skips it, so
	// buildProjectListFrom drops its row and a wholesale RefreshRows would
	// remove it from the picker before the skip marker is installed — the
	// skipped project would vanish instead of being shown as hidden. Graft
	// the picker's last-known row for each skipped ID back in (mirrors
	// preserveSkippedProjectRows for the Projects section); the skip set
	// installed below marks the grafted row hidden.
	skippedIDSet := make(map[string]bool, len(skipped))
	for _, s := range skipped {
		skippedIDSet[s.RepoID] = true
	}
	projects = preserveSkippedPickerRows(projects, m.projectPickerOverlay.Projects(), skippedIDSet)
	m.projectPickerOverlay.RefreshRows(projects)
	m.projectPickerOverlay.SetDegraded(registryDegraded)
	skippedIDs := make([]string, 0, len(skipped))
	hints := make(map[string]string, len(skipped))
	for _, s := range skipped {
		skippedIDs = append(skippedIDs, s.RepoID)
		hints[s.RepoID] = skippedRepoRepairHintForReason(s.Reason)
	}
	m.projectPickerOverlay.SetSkippedRepoIDs(skippedIDs)
	m.projectPickerOverlay.SetSkippedRepoHints(hints)
}

// graftSkippedSidebarRows grafts the Projects section's last-known rows for
// any skipped repo the fresh picker list no longer represents (#4742 review):
// an inactive repo discovered only from its live sessions has no instance
// once the daemon skips it, so buildProjectListFrom drops its row and the
// picker would open without it — closing and reopening Ctrl+P would drop the
// row even while the Projects section still shows it as hidden. Mirrors
// preserveSkippedProjectRows (sidebar) and preserveSkippedPickerRows
// (background refresh); the skip set the caller installs below marks the
// grafted row hidden. No-op when there is nothing to graft from.
func (m *home) graftSkippedSidebarRows(projects []overlay.Project, skipped []daemon.SkippedRepo) []overlay.Project {
	if len(skipped) == 0 || len(m.sidebarProjects) == 0 {
		return projects
	}
	return preserveSkippedPickerRows(projects, m.sidebarProjects, skippedRepoIDSet(skipped))
}

// setPickerSkippedIDsAndHints installs the daemon's startup-skipped repo IDs
// and reason-specific repair hints into an open picker (#4742), so the D path
// gates only on the HIGHLIGHTED project being skipped and the refusal names
// the applicable remedy directly. On a degraded-open path (the synchronous
// fetch failed and returned no skip set) the sidebar may already know a repo
// is skipped from an earlier poll; reuse the retained skip IDs and hints
// instead of leaving the picker with no skip markers while the sidebar still
// shows the repo hidden (#4742 review). Lives here rather than switch_project.go
// to keep that file under the file-length lint (#1145).
func (m *home) setPickerSkippedIDsAndHints(skipped []daemon.SkippedRepo, snapshotDegraded bool) {
	var skippedIDs []string
	var hints map[string]string
	if snapshotDegraded && len(skipped) == 0 && len(m.sidebarProjectsSkipped) > 0 {
		skippedIDs = make([]string, 0, len(m.sidebarProjectsSkipped))
		hints = make(map[string]string, len(m.sidebarProjectsSkippedHints))
		for id, h := range m.sidebarProjectsSkippedHints {
			hints[id] = h
		}
		for id := range m.sidebarProjectsSkipped {
			skippedIDs = append(skippedIDs, id)
		}
	} else {
		skippedIDs = make([]string, 0, len(skipped))
		for _, s := range skipped {
			skippedIDs = append(skippedIDs, s.RepoID)
		}
		hints = make(map[string]string, len(skipped))
		for _, s := range skipped {
			hints[s.RepoID] = skippedRepoRepairHintForReason(s.Reason)
		}
	}
	m.projectPickerOverlay.SetSkippedRepoIDs(skippedIDs)
	m.projectPickerOverlay.SetSkippedRepoHints(hints)
}

// buildProjectListForPicker is buildProjectListWithSnapshotStatus with the
// skipped-row graft folded in (#4742 review): a session-only repo the daemon
// skips has no instance in the fresh snapshot, so buildProjectListFrom drops
// its row and the picker would open without it — closing and reopening Ctrl+P
// would drop the row even while the Projects section still shows it hidden.
// Grafting the sidebar's retained row for each skipped ID (mirrors
// preserveSkippedProjectRows/preserveSkippedPickerRows) keeps it in the open
// picker marked hidden. Lives here rather than switch_project.go to keep that
// file under the file-length lint (#1145).
func (m *home) buildProjectListForPicker() ([]overlay.Project, bool, bool, []daemon.SkippedRepo) {
	projects, registryDegraded, snapshotDegraded, skipped := m.buildProjectListWithSnapshotStatus()
	// On a degraded-open path the synchronous fetch failed, so the daemon's
	// skip set is unknown (buildProjectListWithSnapshotStatus returns nil).
	// The sidebar may already know a repo is skipped from an earlier poll;
	// reuse the retained skip set for grafting so the picker still shows the
	// cached row marked hidden, while SnapshotDegraded stays set to gate the D
	// path on the global fetch-failure refusal (#4742 review). The returned
	// skipped stays nil so the caller can detect the degraded path and reuse
	// the retained hints (a reconstructed []SkippedRepo would lose the reason
	// the hints are derived from).
	graftSkipped := skipped
	if snapshotDegraded && len(skipped) == 0 && len(m.sidebarProjectsSkipped) > 0 {
		graftSkipped = retainedSkippedRepos(m.sidebarProjectsSkipped)
	}
	projects = m.graftSkippedSidebarRows(projects, graftSkipped)
	// A skipped repo the daemon omitted every instance for, and that has no
	// registry/root_agents/active row and no cached sidebar row to graft (a
	// fresh start before the first poll, or a session-only repo first skipped
	// this poll), would otherwise be absent from the picker despite its ID
	// being in SkippedRepos. Synthesize an ID-level degraded row so the
	// operator can see the repo is skipped, not absent (#4742 review).
	projects = ensureSkippedRows(projects, skippedRepoIDSet(graftSkipped))
	return projects, registryDegraded, snapshotDegraded, skipped
}

// retainedSkippedRepos reconstructs the daemon's skipped-repo set from the
// sidebar's retained skip map (#4742 review). Used on the degraded-open picker
// path, where the synchronous fetch failed and buildProjectListWithSnapshotStatus
// returns a nil skip set: the sidebar may already know a repo is skipped from an
// earlier poll, and reusing that set lets the picker graft the cached row and
// mark it hidden while SnapshotDegraded stays set. The Reason is unknown for a
// retained ID, so entries carry an empty Reason; the degraded guard gates D
// globally, so the skip-specific hint is not consulted on this path.
func retainedSkippedRepos(skipped map[string]bool) []daemon.SkippedRepo {
	if len(skipped) == 0 {
		return nil
	}
	out := make([]daemon.SkippedRepo, 0, len(skipped))
	for id := range skipped {
		out = append(out, daemon.SkippedRepo{RepoID: id})
	}
	return out
}

// ensureSkippedRows synthesizes an ID-level degraded row for any skipped repo
// the project list still does not represent (#4742 review). A skipped repo the
// daemon omitted every instance for, and that has no
// registry/root_agents/active row and no cached sidebar row to graft (a fresh
// start, or a session-only repo first skipped this poll), would otherwise be
// absent from the Projects section and a newly opened picker despite its ID
// being in SkippedRepos — leaving the operator no indication the repo exists
// but is skipped. The synthesized row carries only the RepoID (its name IS the
// ID) and is marked hidden via the skip set by projectRows/renderRow; its empty
// Root makes switch (Enter) fail gracefully, and the destructive paths are
// already gated (D by the skip, rebind by the empty RegistryID), so the row is
// informational only. Rows already present for a skipped ID are left untouched.
func ensureSkippedRows(projects []overlay.Project, skipped map[string]bool) []overlay.Project {
	if len(skipped) == 0 {
		return projects
	}
	present := make(map[string]bool, len(projects))
	for _, p := range projects {
		if p.RepoID != "" {
			present[p.RepoID] = true
		}
	}
	for id := range skipped {
		if id == "" || present[id] {
			continue
		}
		projects = append(projects, overlay.Project{RepoID: id, Name: id})
		present[id] = true
	}
	return projects
}

// preserveSkippedPickerRows grafts last-known rows from the open picker for
// any skipped repo the fresh project list no longer represents (#4742 review):
// an inactive repo discovered only from its live sessions has no instance
// once the daemon skips it, so buildProjectListFrom drops its row and a
// wholesale RefreshRows would remove it from the picker before the skip
// marker is installed — the project vanishes instead of being shown as
// hidden. Mirrors preserveSkippedProjectRows for the Projects section; rows
// already present (a registry/root_agents/active-root row for the same id)
// are left untouched.
func preserveSkippedPickerRows(projects, oldRows []overlay.Project, skipped map[string]bool) []overlay.Project {
	if len(skipped) == 0 || len(oldRows) == 0 {
		return projects
	}
	present := make(map[string]bool, len(projects))
	for _, p := range projects {
		present[p.RepoID] = true
	}
	for _, old := range oldRows {
		if old.RepoID == "" || !skipped[old.RepoID] || present[old.RepoID] {
			continue
		}
		projects = append(projects, old)
		present[old.RepoID] = true
	}
	return projects
}
