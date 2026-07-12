package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

func (m *Manager) KillSession(req KillSessionRequest) error {
	instance, repoID, title, data, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return err
	}
	// Canonicalize to the resolved session's title so the killsInFlight key,
	// storage delete, and event all key off the identity we actually resolved
	// (by id), not the request's title. req is a value copy, so this is local.
	req.Title = title
	targetID := killTargetStableID(instance, data)
	// Kill destroys the session unconditionally (#1579). The old unmerged-work
	// guard that refused kills with commits-not-on-base / a dirty worktree / a
	// branch mismatch was dropped by owner decision: it over-refused ordinary
	// cases — most notably squash-merged branches (whose landed commits aren't
	// ancestors of base) and worktrees checked out on a different branch than the
	// stored session branch — blocking routine cleanup. `af sessions archive`
	// remains the non-destructive, restorable default; kill just kills. The
	// worktree-ownership safety (never delete a checkout af doesn't own) is
	// unaffected — it lives in GitWorktree.Cleanup() (external/in-place worktrees
	// are a no-op there), independent of this dropped guard.

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
	instance, repoID, title, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return err
	}
	// Canonicalize to the resolved session's title so the killsInFlight gate and
	// delivery target key off the id-resolved identity, not the request's title.
	req.Title = title
	if instance == nil {
		return fmt.Errorf("failed to restore instance %q", req.Title)
	}

	key := daemonInstanceKey(repoID, req.Title)
	m.mu.Lock()
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing {
		return fmt.Errorf("target session %q is being deleted; prompt not delivered", req.Title)
	}

	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	_, killing = m.killsInFlight[key]
	m.mu.Unlock()
	if killing {
		return fmt.Errorf("target session %q is being deleted; prompt not delivered", req.Title)
	}
	if current == nil {
		return fmt.Errorf("target session %q was deleted; prompt not delivered", req.Title)
	}
	if current != instance {
		if instance.ID != "" && current.ID != "" && current.ID != instance.ID {
			return fmt.Errorf("target session %q changed while preparing prompt; prompt not delivered", req.Title)
		}
		instance = current
	}
	if instance.IsTearingDown() {
		return fmt.Errorf("target session %q is being deleted; prompt not delivered", req.Title)
	}
	if err := promptTargetLivenessError(req.Title, instance.GetLiveness()); err != nil {
		return err
	}
	// Deliver through the agent-server (#1592 Phase 2 PR4), not the tmux-shaped
	// Backend method — the daemon's delivery path is runtime-agnostic. SendPrompt
	// is the reliable command path automated deliveries need.
	if err := instance.AgentServer().SendPrompt(req.Prompt); err != nil {
		return fmt.Errorf("failed to send prompt: %w", err)
	}
	return nil
}

func promptTargetLivenessError(title string, liveness session.Liveness) error {
	switch liveness {
	case session.LiveLost:
		return fmt.Errorf("target session %q is Lost; prompt not delivered; recover it first", title)
	case session.LiveDead:
		return fmt.Errorf("target session %q is Dead; prompt not delivered; recover it first", title)
	case session.LiveArchived:
		// Archived sessions have no live tmux to deliver into (#1529): without
		// this case the prompt falls through to a confusing backend error. Point
		// at the off-ramp, mirroring the TUI's interactiveGuard message. The
		// restore command embeds the title, so shell-quote it — a title with
		// spaces or shell metacharacters must not turn a copy-pasted
		// `af sessions restore ...` into the wrong target or a second command.
		return fmt.Errorf("target session %q is Archived; prompt not delivered; restore it first (af sessions restore %s)", title, shellQuoteArg(title))
	}
	return nil
}

// shellQuoteArg makes s safe to paste as a single shell argument: already-safe
// strings pass through unquoted (so the common `restore captain` stays clean),
// and anything with whitespace/metacharacters is single-quoted with embedded
// quotes escaped. Mirrors the sibling copies in session/tmux/resume.go and
// api/apicmd.go (a shared util is a separate consolidation, #1529).
func shellQuoteArg(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			strings.ContainsRune("_@%+=:,./-", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// agentServerForStream resolves the /v1/sessions/{id}/stream target to its cached
// agent-server for the WS PTY broker (#1592 Phase 2 PR5). The {id} segment is
// resolved by the session's STABLE id first, then by title (with optional repoID)
// as a fallback. The TUI/apiclient pass a title there (no id match → title path,
// behavior unchanged); the browser web client (Phase 5 PR4) passes the
// globally-unique session id, which sidesteps the rail's cross-repo title
// collision — a title alone can name two sessions in two repos, an id names
// exactly one. Both paths return the tracked instance's cached agent-server
// singleton whose ring buffer/subscribers persist.
func (m *Manager) agentServerForStream(idOrTitle, repoID string) (session.AgentServer, error) {
	instance, resolvedRepoID, title, err := m.resolveStreamSession(idOrTitle, repoID)
	if err != nil {
		return nil, err
	}
	if instance == nil {
		return nil, fmt.Errorf("session %q not found", idOrTitle)
	}
	// Reject a new subscription while a kill is in flight for this session, the
	// same killsInFlight gate SendPrompt checks (#1632). Streaming previously
	// skipped it, so a Subscribe could race KillSession's teardown; the
	// agent-server's closed latch is the structural backstop (it refuses to
	// resurrect a broker), and this makes the daemon reject the dial up front
	// rather than opening a stream that immediately EOFs.
	key := daemonInstanceKey(resolvedRepoID, title)
	m.mu.Lock()
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing {
		return nil, fmt.Errorf("session %q is being deleted", title)
	}
	return instance.AgentServer(), nil
}

// resolveStreamSession resolves a stream target by the session's stable id first
// (the web client's key), else by title (the TUI's key, with optional repoID). It
// returns the instance, its resolved repoID, and its title — the last so the
// killsInFlight gate keys off the real title even when the caller addressed the
// session by id. The id scan only sees in-memory (live) instances, which is all a
// stream can attach to; an unmatched id falls through to findSession, which also
// restores an on-disk session the title path may need.
func (m *Manager) resolveStreamSession(idOrTitle, repoID string) (*session.Instance, string, string, error) {
	m.mu.Lock()
	if err := m.refreshLocked(); err != nil {
		m.mu.Unlock()
		return nil, "", "", err
	}
	for key, instance := range m.instances {
		if instance.ID != "" && instance.ID == idOrTitle {
			rid, _ := splitDaemonInstanceKey(key)
			title := instance.Title
			m.mu.Unlock()
			return instance, rid, title, nil
		}
	}
	m.mu.Unlock()
	instance, resolvedRepoID, _, err := m.findSession(idOrTitle, repoID)
	return instance, resolvedRepoID, idOrTitle, err
}

// resolveActionSession resolves a write-action target (kill/archive/send-prompt)
// by the caller-supplied stable id FIRST — the web client's collision-proof key —
// and falls back to {title, repoID} only when no id is given (TUI/CLI callers).
// Resolving by id is what stops a duplicate title across repos from targeting the
// WRONG session on a destructive action: findSession with an empty repoID returns
// the first title match in nondeterministic map order (#1592 Phase 5 follow-up).
//
// It mirrors the stream path's id-first scan (resolveStreamSession): the id scan
// sees only in-memory (live) instances — all a client's rail ever shows — and an
// id that isn't tracked in memory falls through to findSession, which also
// restores a disk-only record the title path may need. It returns the same tuple
// as findSession plus the resolved canonical title, so the caller keys teardown,
// storage, and events off the real session identity rather than the request's
// (possibly stale) title.
func (m *Manager) resolveActionSession(id, title, repoID string) (*session.Instance, string, string, *session.InstanceData, error) {
	if id != "" {
		m.mu.Lock()
		if err := m.refreshLocked(); err != nil {
			m.mu.Unlock()
			return nil, "", "", nil, err
		}
		for key, instance := range m.instances {
			if instance.ID != "" && instance.ID == id {
				rid, _ := splitDaemonInstanceKey(key)
				resolvedTitle := instance.Title
				m.mu.Unlock()
				return instance, rid, resolvedTitle, nil, nil
			}
		}
		m.mu.Unlock()
		// id supplied but not tracked in memory: fall through to the title lookup
		// so a legacy/disk-only record still resolves (the caller sends title too).
	}
	instance, resolvedRepoID, data, err := m.findSession(title, repoID)
	return instance, resolvedRepoID, title, data, err
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

// stableIDFor resolves the stable session id (session.InstanceData.ID, #1195)
// of the tracked session (repoID, title) from the in-memory instance map — the
// same fast-path lookup findSession uses — WITHOUT the disk fallback or restore
// side effects findSession performs. It exists so the control server can stamp
// the stable id onto the delete-class lifecycle events (killed/archived/
// restored), which historically carried only the title and forced clients to
// key their session lists by title — wrong when titles collide across repos
// (#1592 Phase 5 PR5). Returns "" for a session not tracked in memory (a
// legacy/disk-only record that never appears in a live Snapshot, hence never in
// a client's rail); the empty id is the client's title-fallback signal.
func (m *Manager) stableIDFor(repoID, title string) string {
	if title == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if repoID != "" {
		if inst := m.instances[daemonInstanceKey(repoID, title)]; inst != nil {
			return inst.ID
		}
		return ""
	}
	for _, inst := range m.instances {
		if inst.Title == title {
			return inst.ID
		}
	}
	return ""
}
