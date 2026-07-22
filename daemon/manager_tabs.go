package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// CreateTab spawns a Process-kind tab running req.Command in the target
// session's worktree, persists the grown tab list, and returns the resolved tab
// name plus the tmux session it was spawned under (#930 PR 5). The two are
// independent namespaces and diverge after a rename (#1957), so the caller is
// told BOTH rather than left to re-derive the second from the first — see
// CreateTabResponse.TmuxName.
//
// It mirrors CreateSession's discipline: the find+spawn+persist
// runs under the per-repo start lock so a concurrent CreateSession/CreateTab on
// the same repo can't race the tab list or derive a duplicate name. The new tab
// is persisted immediately (ToInstanceData serializes its command + tmux name,
// and restoreLocalTabs reconnects it by exact name on reload) so it survives a
// daemon/af restart — Sachin's hard #930 requirement. Rejected for remote/hook
// instances (no local worktree, and the hook protocol can't run arbitrary
// commands — a remote session's only terminal tab is the terminal_cmd one), an
// empty command, or an instance already at the soft cap (maxTabs, enforced by
// AddProcessTab).
func (m *Manager) CreateTab(req CreateTabRequest) (CreateTabResponse, error) {
	// An empty kind is the default (shell-or-process, per req.Shell), not a kind;
	// every explicit kind resolves through session.ParseTabKindName, the shared
	// vocabulary the CLI validates against, so the two can't drift.
	kind, explicitKind := session.ParseTabKindName(req.Kind)
	if !explicitKind && req.Kind != "" {
		return CreateTabResponse{}, fmt.Errorf("unknown tab kind %q (expected one of %s, or empty)",
			req.Kind, strings.Join(session.TabKindNameList(), ", "))
	}
	isWeb := explicitKind && kind == session.TabKindWeb
	isVSCode := explicitKind && kind == session.TabKindVSCode
	if !explicitKind && !req.Shell && strings.TrimSpace(req.Command) == "" {
		return CreateTabResponse{}, fmt.Errorf("a process tab requires a non-empty command (--command)")
	}
	// A vscode tab always edits the session's own worktree, so a target is not
	// just unnecessary but meaningless — reject one rather than silently ignoring
	// it and leaving the caller thinking it took effect.
	if isVSCode {
		if strings.TrimSpace(req.URL) != "" || req.Port != 0 {
			return CreateTabResponse{}, fmt.Errorf("--url/--port are not valid for a vscode tab (--kind vscode): it always opens the session's worktree")
		}
		if strings.TrimSpace(req.Command) != "" {
			return CreateTabResponse{}, fmt.Errorf("--command is not valid for a vscode tab (--kind vscode)")
		}
	}
	// Resolve the web target up front so an invalid URL/port fails fast, before
	// any session lookup or lock. Only loopback targets are reverse-proxied
	// (webtab_proxy.go); an external URL is iframed directly by the web UI.
	var webURL string
	if isWeb {
		target := strings.TrimSpace(req.URL)
		if target == "" {
			if req.Port == 0 {
				return CreateTabResponse{}, fmt.Errorf("a web tab requires a target (--url or --port)")
			}
			portURL, perr := session.WebTabURLForPort(req.Port)
			if perr != nil {
				return CreateTabResponse{}, perr
			}
			target = portURL
		}
		normalized, nerr := session.NormalizeWebTabURL(target)
		if nerr != nil {
			return CreateTabResponse{}, nerr
		}
		webURL = normalized
	}

	// Resolve by stable id first (req.ID), falling back to {Title, RepoID} — the
	// same id-preferring resolution kill/archive/prompt use, so a web tab-create
	// under a cross-repo title collision can't hit the wrong session (#1592 Phase
	// 5 PR7 / the #1678 class). All downstream lock keys and messages use the
	// RESOLVED title, not the (possibly ambiguous) request title.
	instance, repoID, title, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return CreateTabResponse{}, err
	}
	if instance == nil {
		return CreateTabResponse{}, fmt.Errorf("failed to restore instance %q", title)
	}
	// The message names the real reason rather than the old
	// remote_hooks.terminal_cmd knob, which #1592 Phase 4 PR7 deleted — pointing a
	// user at a setting that no longer exists is worse than no advice (#1874).
	if !instance.Capabilities().TabManagement {
		return CreateTabResponse{}, fmt.Errorf("cannot create a tab on session %q: only local sessions support user-managed tabs — this session's workspace runs off-box (docker/ssh/remote), so there is no local worktree to spawn a tab in", title)
	}

	// Serialize the tab spawn against an archive/kill/restore teardown+move for
	// this session and reject if it is archived/mid-archive (#1195); see
	// archiveExclusiveTabLock for the op-lock ordering and orphan rationale.
	opLock, err := m.archiveExclusiveTabLock(daemonInstanceKey(repoID, title), instance)
	if err != nil {
		return CreateTabResponse{}, err
	}
	defer opLock.Unlock()

	// Serialize against other create/tab mutations on this repo, mirroring
	// CreateSession, so two concurrent CreateTab calls never derive the same name
	// or interleave a spawn-then-persist with another save.
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	// A web tab is pure metadata (a URL, no PTY); a vscode tab is pure metadata
	// too (not even a URL — its code-server is resolved lazily at proxy time); a
	// shell tab runs $SHELL (the TUI `t` mutation, #960 PR 2); a process tab runs
	// the requested command (the CLI/API path, #930 PR 5).
	var tab *session.Tab
	switch {
	case isWeb:
		tab, err = instance.AddWebTab(webURL, req.Name)
	case isVSCode:
		tab, err = instance.AddVSCodeTab(req.Name)
	case req.Shell:
		tab, err = instance.AddShellTab()
	default:
		tab, err = instance.AddProcessTab(req.Command, req.Name)
	}
	if err != nil {
		return CreateTabResponse{}, err
	}

	// Persist through the targeted per-repo writer (persistInstanceData) — the
	// clobber-safe single-writer direction of #960 — rather than a whole-list
	// SaveInstances, which would re-serialize the manager's entire view and was
	// the dual-writer clobber surface PR 4 retires. Mirrors CloseTab/SetPRInfo.
	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		// Roll back the just-spawned tab so a persist failure does not leave a
		// live tmux session that vanishes from the tab list on restart. Carry the
		// new tab's ID into rollback rather than assuming it is still the last slot.
		if closeErr := instance.CloseTabByID(tab.ID); closeErr != nil {
			log.WarningLog.Printf("CreateTab %q: rolling back unpersisted tab failed: %v", title, closeErr)
		}
		return CreateTabResponse{}, fmt.Errorf("failed to persist new tab: %w", err)
	}

	// Announce the grown roster (#1812). A tab created by an agent, the CLI, the
	// TUI, or another browser window is a state change like any other, and
	// without this an already-open web client never learns of it: it only
	// re-Snapshots after its OWN mutation, so a quiet session's tab bar stays
	// stale indefinitely. That silently broke the web tab's stated purpose —
	// letting an agent inject a live browser view into the user's screen.
	//
	// The refreshed InstanceData rides on session.updated rather than a new tab.*
	// event: it already carries the full Tabs roster, and every client re-projects
	// the whole session from it (web's upsertSession, the TUI's
	// ReconcileTabsFromData), so this needs no client change. Published after the
	// persist so no client can observe a tab that isn't durable yet, and while
	// still holding the repo start lock so concurrent tab mutations announce in
	// the same order they persisted. publishEvent is non-blocking (disconnect-slow), so
	// a wedged subscriber can't stall the mutation.
	m.publishEvent(agentproto.EventSessionUpdated, data)
	// Report the tmux session the tab actually spawned under, read out of the very
	// snapshot that was just persisted and keyed on the tab's STABLE ID — not on
	// its ordinal (which any concurrent close shifts) and not by re-deriving
	// "<agent>__<name>", which is the assumption #1957 retired. Empty for a
	// tmux-less kind (web, vscode), which is the honest answer for them.
	return CreateTabResponse{
		ID: tab.ID, Name: tab.Name, TmuxName: tabTmuxNameByID(data.Tabs, tab.ID),
	}, nil
}

// tabTmuxNameByID returns the persisted tmux session name of the tab with this
// stable id, or "" when it has none (a web/vscode tab) or the id is absent.
func tabTmuxNameByID(tabs []session.TabData, id string) string {
	if id == "" {
		return ""
	}
	for _, td := range tabs {
		if td.ID == id {
			return td.TmuxName
		}
	}
	return ""
}

// CloseTab closes a non-agent tab of the target session, kills its tmux
// session, and persists the shrunk tab list (#960 PR 1). It is the close-side
// counterpart of CreateTab. Session resolution, the remote/archived/stale-
// instance guards and the op + repo start locks are tabMutationTarget's — the
// sequence shared with RenameTab/ReorderTab; the persist goes through the
// targeted per-repo writer (persistInstanceData) rather than a whole-list
// SaveInstances, the clobber-safe single-writer direction of #960.
//
// Two of those shared steps carry weight specific to closing. The op-lock:
// archive/kill/restore hold it while closing every tab's tmux session, so
// without it CloseTab can call TmuxSession.Close on the same object concurrently
// (#1434). The archived guard: archive preserves web tabs so a restore can
// render them again, and a tab-delete against an archived session would strip
// that URL out of the record BEFORE the restore meant to bring it back — the
// very loss the preservation exists to prevent, just moved later (#1809).
//
// The tab is resolved by resolveTabTarget, the same precedence every tab verb
// uses: the stable TabID first, then TabName, then TabIndex (#1971). Close is
// where that id earns the most — it is the only DESTRUCTIVE tab verb, so a
// resolve onto a reused name doesn't mislabel a tab, it kills the wrong tmux
// session. The agent tab (index 0) is unclosable — KillSession tears down the
// whole session instead — matching the TUI's `w` rule (handleCloseTab). Returns
// the resolved name of the closed tab. Unlike CreateTab there is no rollback on persist failure: CloseTab
// has already killed the tab's tmux session, so there is nothing live left to
// orphan — the in-memory list (tab removed) is the more accurate state, and the
// stale disk record is harmless (its session is dead and won't reconnect).
func (m *Manager) CloseTab(req CloseTabRequest) (string, error) {
	instance, repoID, title, release, err := m.tabMutationTarget(req.ID, req.Title, req.RepoID,
		tabMutationLabels{action: "close a tab", op: "tab close"})
	if err != nil {
		return "", err
	}
	defer release()

	tabs := instance.GetTabs()
	idx, name, err := resolveTabTarget(tabs, title, req.TabID, req.TabName, req.TabIndex)
	if err != nil {
		return "", err
	}
	if idx == 0 {
		return "", fmt.Errorf("the agent tab of session %q can't be closed; kill the session instead", title)
	}

	closedVSCode := tabs[idx].Kind == session.TabKindVSCode
	targetID, err := stableTabTargetID(tabs[idx], title)
	if err != nil {
		return "", err
	}

	if err := instance.CloseTabByID(targetID); err != nil {
		return "", err
	}

	// The editor is per SESSION, not per tab, so closing one vscode tab only ends
	// it when it was the LAST one — a second vscode tab (or another pane on the
	// same tab) is still using it. Evaluated after the close so the just-removed
	// tab can't count itself, and DEFERRED so it also runs on the persist-failure
	// path below: CloseTab has already removed the tab from the live instance by
	// then, so the editor is unreachable either way and would otherwise linger to
	// daemon shutdown. (If the unpersisted close is undone by a restart, the tab
	// comes back and lazily starts a fresh editor — nothing is lost by stopping.)
	if closedVSCode {
		// The same key tabMutationTarget took its op-lock on: a pure function of the
		// RESOLVED repoID/title it returned, never the request's, so there is exactly
		// one correct value to recompute here.
		key := daemonInstanceKey(repoID, title)
		defer func() {
			if !instanceHasVSCodeTab(instance) {
				m.vscode.stopFor(key)
			}
		}()
	}

	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		return "", fmt.Errorf("failed to persist tab close: %w", err)
	}

	// Announce the shrunk roster (#1812) — the close-side counterpart of
	// CreateTab's publish; see there for why this rides on session.updated. A tab
	// closed out-of-band must disappear from every open client, not just the one
	// that closed it.
	m.publishEvent(agentproto.EventSessionUpdated, data)
	return name, nil
}

// instanceHasVSCodeTab reports whether any of instance's tabs is still a VS Code
// tab, i.e. whether its editor is still needed.
func instanceHasVSCodeTab(instance *session.Instance) bool {
	for _, tab := range instance.GetTabs() {
		if tab.Kind == session.TabKindVSCode {
			return true
		}
	}
	return false
}

// SetPRInfo records (or clears) the GitHub PR info for the target session and
// persists it (#960 PR 1). A zero-value PRInfo (Number 0) clears the recorded
// info. It mirrors CloseTab's discipline — find, take the per-session op-lock so
// a concurrent kill/archive teardown can't replace the session out from under
// us, re-verify the tracked instance hasn't been swapped for a same-titled
// recreate, then mutate+persist under the per-repo start lock through the
// targeted writer (persistInstanceData) — and rolls the in-memory value back on
// persist failure so memory and disk stay consistent. Without the op-lock and
// stale-instance check a SetPRInfo racing KillSession+CreateSession wrote the
// old instance's data (including its stale stable id) over the new instance's
// disk record, corrupting the persisted identity (#1723). This is the
// daemon-side write the TUI performs today via prInfoUpdatedMsg + a full-list
// save (#921); the TUI is switched to it in PR 2.
func (m *Manager) SetPRInfo(req SetPRInfoRequest) error {
	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return err
	}
	if instance == nil {
		return fmt.Errorf("failed to restore instance %q", req.Title)
	}

	// Serialize against an archive/kill/restore teardown for this session and,
	// after winning the lock, confirm the session we resolved is still the tracked
	// current one — a kill/recreate can replace it (same title, DIFFERENT stable
	// id) in the window between findSession and this lock. Take the op-lock before
	// the per-repo start lock, matching the kill/archive persist ordering.
	title := instance.Title
	key := daemonInstanceKey(repoID, title)
	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance || instance.UserKilled() {
		return fmt.Errorf("session %q changed state before PR info could be recorded", title)
	}

	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	var info *git.PRInfo
	if req.PRInfo.Number != 0 {
		info = &git.PRInfo{
			Number: req.PRInfo.Number,
			Title:  req.PRInfo.Title,
			URL:    req.PRInfo.URL,
			State:  req.PRInfo.State,
		}
	}
	prev := instance.GetPRInfo()
	instance.SetPRInfo(info)

	if err := persistInstanceData(repoID, instance.ToInstanceData()); err != nil {
		// Keep memory consistent with disk on a persist failure.
		instance.SetPRInfo(prev)
		return fmt.Errorf("failed to persist PR info: %w", err)
	}
	return nil
}
