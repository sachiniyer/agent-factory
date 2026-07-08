package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// CreateTab spawns a Process-kind tab running req.Command in the target
// session's worktree, persists the grown tab list, and returns the resolved tab
// name (#930 PR 5). It mirrors CreateSession's discipline: the find+spawn+persist
// runs under the per-repo start lock so a concurrent CreateSession/CreateTab on
// the same repo can't race the tab list or derive a duplicate name. The new tab
// is persisted immediately (ToInstanceData serializes its command + tmux name,
// and restoreLocalTabs reconnects it by exact name on reload) so it survives a
// daemon/af restart — Sachin's hard #930 requirement. Rejected for remote/hook
// instances (no local worktree, and the hook protocol can't run arbitrary
// commands — a remote session's only terminal tab is the terminal_cmd one), an
// empty command, or an instance already at the soft cap (maxTabs, enforced by
// AddProcessTab).
func (m *Manager) CreateTab(req CreateTabRequest) (string, error) {
	if !req.Shell && strings.TrimSpace(req.Command) == "" {
		return "", fmt.Errorf("a process tab requires a non-empty command (--command)")
	}

	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return "", err
	}
	if instance == nil {
		return "", fmt.Errorf("failed to restore instance %q", req.Title)
	}
	if instance.IsRemote() {
		return "", fmt.Errorf("cannot create a tab on remote session %q: remote sessions have no local worktree and the hook protocol can't run arbitrary commands; their terminal tab comes from remote_hooks.terminal_cmd", req.Title)
	}

	// Serialize the tab spawn against an archive/kill/restore teardown+move for
	// this session and reject if it is archived/mid-archive (#1195); see
	// archiveExclusiveTabLock for the op-lock ordering and orphan rationale.
	opLock, err := m.archiveExclusiveTabLock(daemonInstanceKey(repoID, req.Title), instance)
	if err != nil {
		return "", err
	}
	defer opLock.Unlock()

	// Serialize against other create/tab mutations on this repo, mirroring
	// CreateSession, so two concurrent CreateTab calls never derive the same name
	// or interleave a spawn-then-persist with another save.
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	// A shell tab runs $SHELL (the TUI `t` mutation, #960 PR 2); a process tab
	// runs the requested command (the CLI/API path, #930 PR 5).
	var tab *session.Tab
	if req.Shell {
		tab, err = instance.AddShellTab()
	} else {
		tab, err = instance.AddProcessTab(req.Command, req.Name)
	}
	if err != nil {
		return "", err
	}

	// Persist through the targeted per-repo writer (persistInstanceData) — the
	// clobber-safe single-writer direction of #960 — rather than a whole-list
	// SaveInstances, which would re-serialize the manager's entire view and was
	// the dual-writer clobber surface PR 4 retires. Mirrors CloseTab/SetPRInfo.
	if err := persistInstanceData(repoID, instance.ToInstanceData()); err != nil {
		// Roll back the just-spawned tab so a persist failure does not leave a
		// live tmux session that vanishes from the tab list on restart.
		if closeErr := instance.CloseTab(instance.TabCount() - 1); closeErr != nil {
			log.WarningLog.Printf("CreateTab %q: rolling back unpersisted tab failed: %v", req.Title, closeErr)
		}
		return "", fmt.Errorf("failed to persist new tab: %w", err)
	}
	return tab.Name, nil
}

// CloseTab closes a non-agent tab of the target session, kills its tmux
// session, and persists the shrunk tab list (#960 PR 1). It is the close-side
// counterpart of CreateTab and mirrors its discipline: find the session, run
// take the per-session op-lock so a concurrent kill/archive teardown can't
// close the same tmux session, run the mutate+persist under the per-repo start
// lock so a concurrent CreateSession/CreateTab/CloseTab on the same repo can't
// interleave with the tab-list write, and persist through the targeted per-repo
// writer
// (persistInstanceData) rather than a whole-list SaveInstances — the
// clobber-safe single-writer direction of #960.
//
// The tab is resolved by TabName when set, otherwise by TabIndex. The agent
// tab (index 0) is unclosable (KillSession tears down the whole session
// instead) and remote sessions' tabs are fixed by their hook config, matching
// the TUI's `w` rule (handleCloseTab). Returns the resolved name of the closed
// tab. Unlike CreateTab there is no rollback on persist failure: CloseTab has
// already killed the tab's tmux session, so there is nothing live left to
// orphan — the in-memory list (tab removed) is the more accurate state, and the
// stale disk record is harmless (its session is dead and won't reconnect).
func (m *Manager) CloseTab(req CloseTabRequest) (string, error) {
	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return "", err
	}
	if instance == nil {
		return "", fmt.Errorf("failed to restore instance %q", req.Title)
	}
	if instance.IsRemote() {
		return "", fmt.Errorf("cannot close a tab on remote session %q: its tabs are fixed by remote_hooks config, not user-managed", req.Title)
	}

	// Serialize the tab close against archive/kill/restore teardown for this
	// session. Those paths hold the same op-lock while closing every tab's tmux
	// session; without this CloseTab can concurrently call TmuxSession.Close on
	// the same object (#1434). Take this before the repo start lock, matching
	// CreateTab and the kill/archive persist ordering.
	key := daemonInstanceKey(repoID, req.Title)
	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	// findSession runs before the op-lock is acquired. If a kill/archive won the
	// lock first, it may have deleted or replaced the tracked instance while we
	// waited; never mutate or re-persist a stale pointer after teardown.
	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance || instance.UserKilled() {
		return "", fmt.Errorf("session %q changed state before tab close could start", req.Title)
	}

	// Serialize against other create/tab mutations on this repo, mirroring
	// CreateTab, so the tab-list mutate+persist never interleaves with another
	// save on the same repo.
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	// Resolve the target tab. TabName takes precedence; otherwise TabIndex.
	tabs := instance.GetTabs()
	idx := req.TabIndex
	name := req.TabName
	if name != "" {
		idx = -1
		for i, tab := range tabs {
			if tab.Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			return "", fmt.Errorf("session %q has no tab named %q", req.Title, name)
		}
	} else {
		if idx < 0 || idx >= len(tabs) {
			return "", fmt.Errorf("session %q has no tab at index %d", req.Title, idx)
		}
		name = tabs[idx].Name
	}
	if idx == 0 {
		return "", fmt.Errorf("the agent tab of session %q can't be closed; kill the session instead", req.Title)
	}

	if err := instance.CloseTab(idx); err != nil {
		return "", err
	}

	if err := persistInstanceData(repoID, instance.ToInstanceData()); err != nil {
		return "", fmt.Errorf("failed to persist tab close: %w", err)
	}
	return name, nil
}

// SetPRInfo records (or clears) the GitHub PR info for the target session and
// persists it (#960 PR 1). A zero-value PRInfo (Number 0) clears the recorded
// info. It mirrors CreateTab's discipline — find, mutate+persist under the
// per-repo start lock, persist through the targeted writer (persistInstanceData)
// — and rolls the in-memory value back on persist failure so memory and disk
// stay consistent. This is the daemon-side write the TUI performs today via
// prInfoUpdatedMsg + a full-list save (#921); the TUI is switched to it in PR 2.
func (m *Manager) SetPRInfo(req SetPRInfoRequest) error {
	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return err
	}
	if instance == nil {
		return fmt.Errorf("failed to restore instance %q", req.Title)
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
