package daemon

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

func (m *Manager) KillSession(req KillSessionRequest) error {
	instance, repoID, data, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return err
	}
	targetID := killTargetStableID(instance, data)

	key := daemonInstanceKey(repoID, req.Title)
	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return fmt.Errorf("kill already in progress for session %q", req.Title)
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	// Serialize against a Lost-recovery in flight for this session (#1108
	// PR 2): a kill arriving mid-Recover waits for the recover attempt to
	// finish and then tears the (possibly just-restored) session down —
	// never an interleaved teardown-vs-respawn. killsInFlight is registered
	// BEFORE this acquire, so the restore loop's in-lock re-check sees the
	// kill intent and aborts instead of racing to go first.
	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	if m.currentInstanceReplaced(key, instance, targetID) {
		log.InfoLog.Printf("kill of session %q skipped: current instance identity changed before teardown", req.Title)
		return nil
	}

	// Persist the kill-intent tombstone BEFORE teardown begins (#1108): if the
	// daemon dies or the teardown errors between here and DeleteInstance, the
	// surviving record is provably a user kill — the status poll finishes the
	// teardown instead of classifying the vanished session Lost and restoring
	// it. Best-effort: a failed tombstone write degrades to today's crash
	// window, which must not block the kill itself.
	m.persistKillTombstone(repoID, instance, data)

	if instance != nil {
		if err := instance.Kill(); err != nil {
			return fmt.Errorf("failed to kill instance: %w", err)
		}
	} else if data != nil {
		ghostCleanup(data, req.Title)
	}

	state := config.LoadState()
	storage, err := session.NewStorage(state, repoID)
	if err != nil {
		return err
	}
	deleted, err := storage.DeleteInstanceByStableID(req.Title, targetID)
	if err != nil {
		return fmt.Errorf("failed to delete instance from storage: %w", err)
	}
	if !deleted {
		log.InfoLog.Printf("kill of session %q skipped storage delete: current record has a different instance identity", req.Title)
		return nil
	}

	m.mu.Lock()
	if current := m.instances[key]; current == nil || current == instance || stableIDMatchesForDaemon(current.ID, targetID) {
		delete(m.instances, key)
	}
	if session.IsReservedTitle(req.Title) {
		// An explicit kill is honored only briefly: the ensure loop suppresses
		// re-creation for rootKillHealDelay, then self-heals a still-configured
		// root (#1223). Config (root_agents) is the source of truth — removing
		// the repo from it is the only permanent stop. Recorded even for
		// unconfigured repos (harmless — the loop never visits them — and it
		// keeps kill-vs-config-change ordering race-free).
		m.rootKilledAt[repoID] = nowFunc()
		log.InfoLog.Printf("root agent for repo %s killed by user; the ensure loop will re-create it in ~%s unless the repo is removed from root_agents", repoID, rootKillHealDelay)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) SendPrompt(req SendPromptRequest) error {
	if req.Prompt == "" {
		return fmt.Errorf("prompt is required")
	}
	instance, _, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return err
	}
	if instance == nil {
		return fmt.Errorf("failed to restore instance %q", req.Title)
	}
	if err := instance.SendPromptCommand(req.Prompt); err != nil {
		return fmt.Errorf("failed to send prompt: %w", err)
	}
	return nil
}

func (m *Manager) findSession(title, repoID string) (*session.Instance, string, *session.InstanceData, error) {
	if title == "" {
		return nil, "", nil, fmt.Errorf("session title is required")
	}

	m.mu.Lock()
	if err := m.refreshLocked(); err != nil {
		m.mu.Unlock()
		return nil, "", nil, err
	}
	if repoID != "" {
		key := daemonInstanceKey(repoID, title)
		if instance := m.instances[key]; instance != nil {
			m.mu.Unlock()
			return instance, repoID, nil, nil
		}
	} else {
		for key, instance := range m.instances {
			if instance.Title == title {
				rid, _ := splitDaemonInstanceKey(key)
				m.mu.Unlock()
				return instance, rid, nil, nil
			}
		}
	}
	m.mu.Unlock()

	data, rid, err := findInstanceDataByTitle(title, repoID)
	if err != nil {
		return nil, "", nil, err
	}
	instance, restoreErr := fromInstanceDataForRefresh(*data)
	if restoreErr != nil {
		return nil, rid, data, nil
	}

	// We built `instance` from disk with m.mu released, so a concurrent
	// refresh (or another RPC) may have restored and registered the canonical
	// Instance for this session during the window (#867). Returning our freshly
	// built duplicate would hand the caller an *untracked* Instance: SendPrompt
	// would leak its restore-time attach PTY, and KillSession would call
	// instance.Kill() — tearing down the tmux session and worktree that the
	// canonical, still-tracked Instance shares. Re-acquire the lock and:
	//   - if a tracked Instance now exists, drop our duplicate (closing only
	//     its attach resources, never the shared session) and operate on the
	//     tracked one; otherwise
	//   - register our Instance so callers operate on a tracked Instance, just
	//     as the refresh loop would have, instead of an orphan.
	key := daemonInstanceKey(rid, title)
	m.mu.Lock()
	if tracked := m.instances[key]; tracked != nil {
		m.mu.Unlock()
		if err := instance.CloseAttachOnly(); err != nil {
			log.WarningLog.Printf("findSession %q: closing duplicate instance attach failed: %v", title, err)
		}
		return tracked, rid, data, nil
	}
	// Match the refresh loop: instances the daemon tracks always run AutoYes.
	instance.SetAutoYes(true)
	m.instances[key] = instance
	m.mu.Unlock()
	return instance, rid, data, nil
}

func (m *Manager) ImportRemoteHookSessions(req ImportRemoteHookSessionsRequest) ([]session.InstanceData, error) {
	if req.RepoPath == "" {
		return nil, fmt.Errorf("repo path is required")
	}
	repo, err := config.RepoFromPath(req.RepoPath)
	if err != nil {
		return nil, err
	}
	repoCfg, err := config.ResolveConfig(repo.Root)
	if err != nil {
		return nil, err
	}
	if repoCfg.RemoteHooks == nil || repoCfg.RemoteHooks.ListCmd == "" {
		return nil, nil
	}

	listed, err := session.ListRemoteHookInstanceData(repo.Root, *repoCfg.RemoteHooks, time.Now())
	if err != nil {
		return nil, err
	}

	imported := make([]session.InstanceData, 0, len(listed))
	if err := config.UpdateRepoInstances(repo.ID, func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		existingTitles := make(map[string]bool, len(existing))
		existingHookNames := make(map[string]bool)
		for _, data := range existing {
			existingTitles[data.Title] = true
			if data.BackendType == "remote" {
				existingHookNames[session.RemoteHookName(data.Title, data.RemoteMeta)] = true
			}
		}
		for _, data := range listed {
			name := session.RemoteHookName(data.Title, data.RemoteMeta)
			if existingTitles[data.Title] || existingHookNames[name] {
				continue
			}
			existing = append(existing, data)
			imported = append(imported, data)
			existingTitles[data.Title] = true
			existingHookNames[name] = true
		}
		return json.Marshal(existing)
	}); err != nil {
		return nil, err
	}

	m.mu.Lock()
	_ = m.refreshLocked()
	m.mu.Unlock()
	return imported, nil
}
