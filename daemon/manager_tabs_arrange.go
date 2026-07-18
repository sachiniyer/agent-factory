package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// Tab arrangement RPCs: rename and reorder (#1813). Where CreateTab/CloseTab
// change WHICH tabs exist (and so spawn or kill a tmux session), these change
// only how the existing roster is labelled and ordered. Both are pure metadata
// mutations, which is what shapes their two differences from CloseTab: they
// touch no tmux session, and they roll back on a persist failure (see the
// rollback note below).

// tabMutationLabels supplies the verb-specific fragments of the guard messages
// tabMutationTarget emits. The guard SEQUENCE is identical for every in-place
// tab mutation and belongs in one place; the guard TEXT must still name the verb
// the user actually ran, so it is parameterized rather than generalized into one
// vague string.
type tabMutationLabels struct {
	// action reads after "cannot ": "close a tab", "rename a tab".
	action string
	// op reads after "changed state before ": "tab close", "tab rename".
	op string
}

// errTabMutationArchived is the refusal an in-place tab mutation returns for an
// archived session. It is shared by tabMutationTarget's pre-lock fast path and
// its post-lock gate so a mutation that LOSES the race to an archive is
// indistinguishable — to a CLI user or the web UI — from one that arrived after
// it: same refusal, same advice, roster intact.
func errTabMutationArchived(action, title string) error {
	return fmt.Errorf("cannot %s on archived session %q; restore it first (af sessions restore)", action, title)
}

// tabMutationTarget resolves the session an in-place tab mutation addresses,
// runs every guard such a mutation must pass, and takes its locks in the
// canonical order. It returns the instance, its repo id, its RESOLVED title, and
// a release func the caller MUST defer.
//
// This is the sequence CloseTab established and every tab verb owes:
//
//   - resolveActionSession resolves by stable id FIRST, falling back to
//     {Title, RepoID}, so a mutation can't hit a same-titled session in another
//     repo (#1592 Phase 5 PR7 / the #1678 class). Everything downstream — lock
//     keys, messages, the persist — uses the RESOLVED title, never the request's.
//   - Off-box sessions are refused: their tab list is fixed by their runtime,
//     not by user edits (#1874).
//   - Archived sessions are refused TWICE: archive is inert in BOTH directions
//     (#1809), so a mutation can't rewrite a record the restore is meant to bring
//     back intact. The pre-lock check is only a fast path; the post-lock one is
//     the real gate (see below).
//   - The per-session op-lock serializes against an archive/kill/restore
//     teardown, then — because resolveActionSession ran BEFORE that lock — the
//     tracked instance is re-read and rejected if a kill/recreate swapped it
//     while we waited. Without that re-read a mutation racing
//     KillSession+CreateSession persists the dead instance's data (stale stable
//     id included) over the live one's record (#1723).
//   - Archived is then re-checked UNDER the op-lock, because the pointer re-read
//     above cannot see an archive: ArchiveSession holds this SAME op-lock, commits
//     LiveArchived, and leaves the same instance tracked (an archived row stays
//     listed and restorable). So a mutation that resolved a live session and then
//     queued behind an archive arrives with current == instance and UserKilled()
//     false — both checks pass — and would rewrite the roster the archive just
//     preserved, then persist the loss. That is the #1809 loss exactly, reached
//     through the race rather than the front door.
//   - The per-repo start lock serializes the mutate+persist against
//     CreateSession/CreateTab/CloseTab on the same repo.
//
// Taking the op-lock before the repo start lock matches CreateTab/CloseTab and
// the kill/archive persist ordering; release unwinds in reverse.
func (m *Manager) tabMutationTarget(reqID, reqTitle, reqRepoID string, labels tabMutationLabels) (*session.Instance, string, string, func(), error) {
	instance, repoID, title, _, _, err := m.resolveActionSession(reqID, reqTitle, reqRepoID)
	if err != nil {
		return nil, "", "", nil, err
	}
	if instance == nil {
		return nil, "", "", nil, fmt.Errorf("failed to restore instance %q", title)
	}
	if !instance.Capabilities().TabManagement {
		return nil, "", "", nil, fmt.Errorf("cannot %s on session %q: its tab list is fixed by its runtime, not user-managed — this session's workspace runs off-box (docker/ssh/remote)", labels.action, title)
	}
	// Fast path only: reject an already-archived session without making the caller
	// wait on the op-lock. The authoritative check is the post-lock one below.
	if instance.IsArchived() {
		return nil, "", "", nil, errTabMutationArchived(labels.action, title)
	}

	key := daemonInstanceKey(repoID, title)
	opLock := m.opLockFor(key)
	opLock.Lock()

	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance || instance.UserKilled() {
		opLock.Unlock()
		return nil, "", "", nil, fmt.Errorf("session %q changed state before %s could start", title, labels.op)
	}
	// The real archived gate — see the doc comment for why the checks above cannot
	// see an archive that won the op-lock.
	if instance.IsArchived() {
		opLock.Unlock()
		return nil, "", "", nil, errTabMutationArchived(labels.action, title)
	}

	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()

	return instance, repoID, title, func() {
		repoStartLock.Unlock()
		opLock.Unlock()
	}, nil
}

// resolveTabTarget resolves which tab of tabs a request addresses, in strict
// precedence: the stable tabID first, then TabName, then TabIndex. Returns the
// tab's index and its current name. Shared by every tab verb so a tab is
// addressed identically — and reports a miss identically — no matter which verb
// addressed it.
//
// The id comes first because it is the only handle that is not REUSABLE (#1929).
// A name is freed by a close and handed to the next tab that requests it; an
// index shifts on every close and reorder. So a client that resolves a tab and
// then sends its name is asking for "whatever is called that NOW", which after a
// concurrent close+create or rename is a different tab — and the mutation lands
// on it silently, because a name-keyed resolve does not fail, it succeeds on the
// wrong tab. The stable id (#1738) is minted once and never reused, so it means
// the tab the client actually looked at.
//
// A non-empty id that no longer resolves is REFUSED, never fallen back to the
// name or index. This is the #1779 rule that Preview already owes: falling back
// would address whatever tab has since taken the name — the precise misroute the
// id exists to prevent — so a fallback would reopen the race in the one case it
// most matters. The name/index carried alongside a resolving id are ignored
// rather than cross-checked: the name changing underneath the client is the
// normal case this field exists to survive, so a mismatch is not an error.
//
// The id is matched against the SAME tabs slice the caller goes on to index,
// rather than via instance.TabIndexByID: the caller resolves against a snapshot
// it already holds, and a second lookup against the live list could return an
// index that no longer addresses the same element of tabs.
func resolveTabTarget(tabs []*session.Tab, title, tabID, tabName string, tabIndex int) (int, string, error) {
	if tabID != "" {
		for i, tab := range tabs {
			if tab.ID == tabID {
				return i, tab.Name, nil
			}
		}
		return 0, "", fmt.Errorf("session %q has no tab with id %q; it may have been closed — reload the session's tabs and retry", title, tabID)
	}
	if tabName != "" {
		// Match the canonical Name only (session.TabMatches, #1986). The display
		// label is presentation and is never an identifier: the TUI shows
		// "Terminal" for a tab named "shell", but the label does not resolve here —
		// accepting it would make two strings address one tab, the ambiguity #1929/
		// #1904 removed from the tab surface. A user who typed the label is not left
		// stranded: the miss error below names the real handle. Because this is the
		// shared resolver (#1971), close, rename, and reorder all key on Name alike.
		for i, tab := range tabs {
			if session.TabMatches(tab, tabName) {
				return i, tab.Name, nil
			}
		}
		// Name the tabs that DO exist, with their labels where the two differ, so a
		// user who read "Terminal" learns to type "shell". The old error asserted an
		// absence and left the user to guess the mapping; listing the valid options
		// turns a dead end into a fix, and still fires correctly for a real typo.
		ids := make([]string, 0, len(tabs))
		for _, tab := range tabs {
			ids = append(ids, session.TabIdentifiers(tab))
		}
		return 0, "", fmt.Errorf("session %q has no tab named %q; its tabs are: %s",
			title, tabName, strings.Join(ids, ", "))
	}
	if tabIndex < 0 || tabIndex >= len(tabs) {
		return 0, "", fmt.Errorf("session %q has no tab at index %d", title, tabIndex)
	}
	return tabIndex, tabs[tabIndex].Name, nil
}

// Rollback on persist failure: rename and reorder DO roll back, CloseTab
// deliberately does not, and CreateTab does. Since the existing verbs resolve
// this in opposite directions, a new tab verb has to decide rather than copy.
//
// The rule is what the mutation left OUTSIDE memory. CreateTab rolls back
// because it has already spawned a live tmux session: keeping it after a failed
// persist would orphan a process that vanishes from the roster on restart.
// CloseTab does NOT roll back because it has already killed the tab's tmux
// session — that is irreversible, so the in-memory list (tab gone) is the more
// accurate state and the stale disk record is harmless (its session is dead and
// won't reconnect).
//
// Rename and reorder are the third case: pure in-memory metadata, no tmux, no
// side effect, and exactly reversible. So memory can and should be put back —
// leaving it diverged would show the user a name/order that silently reverts on
// the next restart, which reads as data loss. This matches SetPRInfo, the other
// pure-metadata mutation.

// RenameTab relabels one tab of the target session, persists the roster, and
// returns the RESOLVED name (#1813). It mirrors CloseTab's discipline via
// tabMutationTarget, and adds the two guards that are specific to renaming.
//
// Only tabs that actually display their name may be renamed
// (session.TabKindRenameable): agent tabs always render "Agent" and shell tabs
// always render "Terminal" on every surface, so a rename would write a field
// nothing reads. Refusing with an actionable message beats a success the user
// can't see.
//
// The resolved name is the sanitized, collision-suffixed one the tab actually
// received, which is what clients must render and what other verbs now address
// the tab by.
func (m *Manager) RenameTab(req RenameTabRequest) (string, error) {
	instance, repoID, title, release, err := m.tabMutationTarget(req.ID, req.Title, req.RepoID,
		tabMutationLabels{action: "rename a tab", op: "tab rename"})
	if err != nil {
		return "", err
	}
	defer release()

	tabs := instance.GetTabs()
	idx, prevName, err := resolveTabTarget(tabs, title, req.TabID, req.TabName, req.TabIndex)
	if err != nil {
		return "", err
	}
	if idx == 0 {
		return "", fmt.Errorf("the agent tab of session %q can't be renamed: it always displays as %q", title, "Agent")
	}
	if !session.TabKindRenameable(tabs[idx].Kind) {
		return "", fmt.Errorf("tab %q of session %q can't be renamed: shell tabs always display as %q. Only web, process and VS Code tabs show a custom name — create one with 'af sessions tab-create --kind web' or '--command'", prevName, title, "Terminal")
	}

	name, err := instance.RenameTab(idx, req.NewName)
	if err != nil {
		return "", err
	}

	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		// Put the old name back so memory matches disk (see the rollback note
		// above). Both
		// locks are still held, so prevName cannot have been taken in the window
		// and resolves back exactly; a surprise means the roster changed underneath
		// us, which is worth a log line rather than a silent wrong name.
		if got, rerr := instance.RenameTab(idx, prevName); rerr != nil || got != prevName {
			log.WarningLog.Printf("RenameTab %q: rolling back unpersisted rename of tab %q returned %q, %v", title, prevName, got, rerr)
		}
		return "", fmt.Errorf("failed to persist tab rename: %w", err)
	}

	// Announce the relabelled roster (#1812). A rename is a state change like a
	// create or close: without this a second browser window keeps rendering the
	// old name indefinitely, because a client only re-Snapshots after its OWN
	// mutation. Published after the persist so no client can observe a name that
	// isn't durable yet, and while the repo start lock is still held so concurrent
	// tab mutations announce in the order they persisted. See CreateTab's publish
	// for why this rides on session.updated rather than a new tab.* event.
	m.publishEvent(agentproto.EventSessionUpdated, data)
	return name, nil
}

// ReorderTab moves one tab within the target session's roster, persists the new
// order, and returns the moved tab's name and final index (#1813). It mirrors
// CloseTab's discipline via tabMutationTarget, and pins index 0 in both
// directions.
//
// The agent tab cannot be moved and nothing may be moved in front of it: the
// session package identifies the agent tab POSITIONALLY as Tabs[0] (archive
// teardown keeps Tabs[0]; the agent conversation and the agent tmux session are
// read off it), so permuting slot 0 would silently re-point all of that at a
// shell tab. This is a correctness invariant, not a display preference.
func (m *Manager) ReorderTab(req ReorderTabRequest) (string, int, error) {
	instance, repoID, title, release, err := m.tabMutationTarget(req.ID, req.Title, req.RepoID,
		tabMutationLabels{action: "reorder the tabs", op: "tab reorder"})
	if err != nil {
		return "", 0, err
	}
	defer release()

	tabs := instance.GetTabs()
	idx, name, err := resolveTabTarget(tabs, title, req.TabID, req.TabName, req.TabIndex)
	if err != nil {
		return "", 0, err
	}
	if idx == 0 {
		return "", 0, fmt.Errorf("the agent tab of session %q can't be moved: it is pinned to the first slot", title)
	}
	if req.NewIndex == 0 {
		return "", 0, fmt.Errorf("cannot move tab %q of session %q to index 0: that slot is reserved for the agent tab", name, title)
	}
	if req.NewIndex < 0 || req.NewIndex >= len(tabs) {
		return "", 0, fmt.Errorf("session %q has no tab slot at index %d (valid: 1-%d)", title, req.NewIndex, len(tabs)-1)
	}

	if err := instance.ReorderTab(idx, req.NewIndex); err != nil {
		return "", 0, err
	}

	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		// Put the original order back (see the rollback note above). Moving the tab from its
		// new index back to its old one is the exact inverse of the move above.
		if rerr := instance.ReorderTab(req.NewIndex, idx); rerr != nil {
			log.WarningLog.Printf("ReorderTab %q: rolling back unpersisted move of tab %q failed: %v", title, name, rerr)
		}
		return "", 0, fmt.Errorf("failed to persist tab reorder: %w", err)
	}

	// Announce the reordered roster (#1812) — see RenameTab's publish.
	m.publishEvent(agentproto.EventSessionUpdated, data)
	return name, req.NewIndex, nil
}
