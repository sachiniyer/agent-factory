// Session-target resolution: turn a caller-supplied identifier — a stable id, a
// {repo, title} pair, or an unscoped Web id — into the *session.Instance the
// daemon actually tracks (or a disk-restored stand-in) plus the repo/title it
// resolved under. The streamTarget value type keeps the three request shapes
// mutually exclusive so no resolver phase can give a foreign id precedence over
// an explicit repo scope; the tracked*Locked helpers answer from m.instances
// alone for paths that must not let a miss turn into I/O; and
// resolveActionSession/findSessionByStableID are the id-first, ambiguity-guarded
// substrate the kill/archive/send-prompt mutations resolve through.
// manager_sessions.go keeps the kill and send-prompt operations that consume it.
package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// agentServerForStream resolves the /v1/sessions/{id}/stream target to its cached
// agent-server for the WS PTY broker (#1592 Phase 2 PR5). The route multiplexes
// two authoritative request shapes: TUI/CLI send {title}+repo_id, while Web sends
// a globally unique stable id with no repo_id. Presence of repo_id selects the
// title namespace; only the unscoped shape tries stable identity first. Both
// return the tracked instance's cached agent-server singleton whose ring
// buffer/subscribers persist.
func (m *Manager) agentServerForStream(idOrTitle, repoID string) (session.AgentServer, *session.Instance, error) {
	return m.agentServerForStreamTarget(authoritativeStreamTarget(idOrTitle, repoID))
}

// agentServerForStreamTarget resolves a target to its agent-server without any
// liveness gate: Preview and WorkspaceFacts measure rows an attach would refuse
// (an Archived session's preview, a remote row whose endpoint recovered before
// the next status reconciliation), so they must keep resolving them.
func (m *Manager) agentServerForStreamTarget(target streamTarget) (session.AgentServer, *session.Instance, error) {
	return m.resolveAgentServerForTarget(target, false)
}

// agentServerForAttachTarget is agentServerForStreamTarget for the attach paths
// only (/stream and /stream-info): it additionally refuses a non-live target
// before the WS upgrade, with the restore off-ramp.
func (m *Manager) agentServerForAttachTarget(target streamTarget) (session.AgentServer, *session.Instance, error) {
	return m.resolveAgentServerForTarget(target, true)
}

func (m *Manager) resolveAgentServerForTarget(target streamTarget, attach bool) (session.AgentServer, *session.Instance, error) {
	instance, resolvedRepoID, title, err := m.resolveStreamTarget(target)
	if err != nil {
		return nil, nil, err
	}
	if instance == nil {
		name := target.title
		if name == "" {
			name = target.stableID
		}
		return nil, nil, fmt.Errorf("session %q not found", name)
	}
	// Attach only: refuse a non-live target with the same actionable off-ramp
	// SendPrompt's promptTargetLivenessError returns (#1529). The stream path
	// resolves any title-matching row with no liveness filter, so an Archived (or
	// Lost/Dead) session would upgrade the WS and then fail deep at broker
	// subscribe with a generic 500 whose body the client discards. Refusing here
	// keeps the failure BEFORE websocket.Accept. streamTargetLivenessError keeps
	// the refusal inside the 1 KiB the WS dial client reads.
	if attach {
		if err := streamTargetLivenessError(title, instance.GetLiveness()); err != nil {
			return nil, nil, err
		}
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
		return nil, nil, fmt.Errorf("session %q is being deleted", title)
	}
	return instance.AgentServer(), instance, nil
}

// resolveStreamSession returns the instance, its resolved repoID, and its title —
// the last so the killsInFlight gate keys off the real title even when Web used a
// stable id. Both request shapes consult the daemon's already-restored map
// directly: Preview is a repaint hot path, so re-scanning every persisted session
// before each capture makes its latency grow with the whole session history. A
// miss refreshes exactly once, using the SAME streamTarget authority in both
// phases: repo-scoped title lookup never detours through the global ID namespace,
// while an unscoped Web ID gets first refusal after refresh before legacy title
// fallback (#2187/#2279 review).
func (m *Manager) resolveStreamSession(idOrTitle, repoID string) (*session.Instance, string, string, error) {
	return m.resolveStreamTarget(authoritativeStreamTarget(idOrTitle, repoID))
}

func (m *Manager) resolveStreamTarget(target streamTarget) (*session.Instance, string, string, error) {
	if target.idOnly {
		return m.resolveStreamSessionByIDOnly(target.stableID)
	}
	m.mu.Lock()
	if instance, rid, title := m.trackedStreamSessionLocked(target); instance != nil {
		m.mu.Unlock()
		return instance, rid, title, nil
	}
	m.mu.Unlock()
	instance, resolvedRepoID, _, err := m.findSessionByStableID(target.stableID, target.title, target.repoID)
	if instance == nil {
		return nil, resolvedRepoID, target.title, err
	}
	return instance, resolvedRepoID, instance.Title, err
}

// resolveStreamSessionByIDOnly is the by=id shape: the tracked map, then one
// refresh, then the same id lookup again — and a miss is a refusal. It never
// consults the title namespace, because the caller has told us the segment is an
// id: a stale one reinterpreted as a title could open an input stream to a
// DIFFERENT session (#4760 review).
func (m *Manager) resolveStreamSessionByIDOnly(id string) (*session.Instance, string, string, error) {
	if id == "" {
		return nil, "", "", fmt.Errorf("session id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if instance, rid := m.trackedSessionByIDLocked(id); instance != nil {
		return instance, rid, instance.Title, nil
	}
	if err := m.refreshLocked(); err != nil {
		return nil, "", "", err
	}
	if instance, rid := m.trackedSessionByIDLocked(id); instance != nil {
		return instance, rid, instance.Title, nil
	}
	return nil, "", "", fmt.Errorf("session with id %q %w", id, errSessionNotFound)
}

// streamTarget makes the route's authority shapes mutually exclusive. A scoped
// target can carry only {repo,title}; an unscoped target may carry the
// stable-id interpretation of the same opaque segment; an id-only target
// carries no title at all. No resolver phase can accidentally give a foreign
// global ID precedence over an explicit repo scope, nor a title precedence
// over an explicit id.
type streamTarget struct {
	stableID string
	title    string
	repoID   string
	idOnly   bool
}

func authoritativeStreamTarget(idOrTitle, repoID string) streamTarget {
	target := streamTarget{title: idOrTitle, repoID: repoID}
	if repoID == "" {
		target.stableID = idOrTitle
	}
	return target
}

// trackedStreamSessionLocked resolves only facts already materialized in
// m.instances. The caller holds m.mu. The streamTarget, rather than this helper,
// decides the namespace so the hot path and post-refresh path cannot drift.
// trackedStreamSession resolves idOrTitle against the ALREADY-RESTORED instance map
// and nothing else — no disk read, no refresh, no fallback. It is for callers that
// must not let a lookup miss turn into I/O, because the lookup is reachable by an
// untrusted peer: /v1/preview-auth's mint guard is a GET that a cross-origin page can
// drive under the tokenless-loopback default, so a refreshing miss path there is an
// amplifier rather than a convenience (#2833 review).
//
// Callers that legitimately need a session the daemon has not tracked yet keep using
// resolveStreamSession; this is deliberately the weaker, cheaper question.
func (m *Manager) trackedStreamSession(idOrTitle string) *session.Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	instance, _, _ := m.trackedStreamSessionLocked(authoritativeStreamTarget(idOrTitle, ""))
	return instance
}

func (m *Manager) trackedStreamSessionLocked(target streamTarget) (*session.Instance, string, string) {
	if target.repoID != "" {
		if instance := m.instances[daemonInstanceKey(target.repoID, target.title)]; instance != nil {
			return instance, target.repoID, instance.Title
		}
		return nil, "", ""
	}
	if instance, rid := m.trackedSessionByIDLocked(target.stableID); instance != nil {
		return instance, rid, instance.Title
	}
	return nil, "", ""
}

// trackedSessionByIDLocked resolves one stable identity in the materialized
// instance map. The caller holds m.mu. Stream hot-path lookup and post-refresh
// lookup share it so the stable namespace cannot drift between the two phases.
func (m *Manager) trackedSessionByIDLocked(id string) (*session.Instance, string) {
	if id == "" {
		return nil, ""
	}
	for key, instance := range m.instances {
		if instance != nil && instance.ID == id {
			rid, _ := splitDaemonInstanceKey(key)
			return instance, rid
		}
	}
	return nil, ""
}

// resolveActionSession resolves a session mutation target by the caller-supplied
// stable id FIRST — the retained clients' collision-proof key — and falls back
// to {title, repoID} only when NO id is given (legacy/one-shot CLI callers).
// Resolving by id is what stops a duplicate title across repos from targeting the
// WRONG session on a destructive action: findSession with an empty repoID returns
// the first title match in nondeterministic map order (#1592 Phase 5 follow-up).
//
// A supplied id is AUTHORITATIVE — it uniquely names one session. If it is not
// tracked in memory (the session was killed/archived out from under a stale client
// rail), this returns a clear "not found" error rather than falling back to a title
// match: an empty-repoID title lookup could resolve a DIFFERENT same-title session
// in another repo and operate on it — the exact destructive cross-repo collision
// this fix closes, just re-entered through a stale id. Erroring keeps a stale id
// from ever silently retargeting; the id-less title path stays for legacy/CLI.
//
// Its ID-addressed arm mirrors the unscoped Web stream shape; repo-scoped stream
// callers deliberately take the title arm instead. It returns the resolved
// instance, its repoID, its canonical title, its stable id, and (for the title
// path) its on-disk data — so the caller keys teardown, storage, AND the lifecycle
// event off the session actually resolved, never the request's own (possibly
// stale) id/title.
func (m *Manager) resolveActionSession(id, title, repoID string) (*session.Instance, string, string, string, *session.InstanceData, error) {
	if id != "" {
		m.mu.Lock()
		if err := m.refreshLocked(); err != nil {
			m.mu.Unlock()
			return nil, "", "", "", nil, err
		}
		for key, instance := range m.instances {
			if instance.ID != "" && instance.ID == id {
				rid, _ := splitDaemonInstanceKey(key)
				resolvedTitle := instance.Title
				m.mu.Unlock()
				return instance, rid, resolvedTitle, instance.ID, nil, nil
			}
		}
		m.mu.Unlock()
		return nil, "", "", "", nil, fmt.Errorf("session with id %q %w", id, errSessionNotFound)
	}
	// Legacy/CLI path: no id supplied, resolve by {title, repoID}.
	instance, resolvedRepoID, data, err := m.findSession(title, repoID)
	if err != nil {
		return nil, "", "", "", nil, err
	}
	resolvedID := ""
	if instance != nil {
		resolvedID = instance.ID
	} else if data != nil {
		resolvedID = data.ID
	}
	return instance, resolvedRepoID, title, resolvedID, data, nil
}

func (m *Manager) findSession(title, repoID string) (*session.Instance, string, *session.InstanceData, error) {
	return m.findSessionByStableID("", title, repoID)
}

// findSessionByStableID performs findSession's refresh exactly once, then gives
// stableID first refusal on the refreshed map before any title lookup. Passing an
// empty stableID preserves findSession's title-only behavior. The stream route
// supplies stableID only for its unscoped shape; this both prevents a newly
// restored Web ID from being reinterpreted as somebody else's title (#2187) and
// prevents a scoped TUI title from being reinterpreted as a foreign ID (#2279
// review).
// collectTitleRepoPaths is the disk half of findSession's cross-project
// ambiguity guard. A package var so tests can drive each of its three outcomes
// — matches found, gaps reported, enumeration failed — in isolation: the
// daemon's load gate refuses an unreadable record before findSession runs, so
// the gap branch is otherwise unreachable from a fixture and would go untested.
// Mirrors branchesHeldByWorktrees' precedent. Production never reassigns it.
var collectTitleRepoPaths = collectTitleRepoPathsOnDisk

func (m *Manager) findSessionByStableID(stableID, title, repoID string) (*session.Instance, string, *session.InstanceData, error) {
	if title == "" {
		return nil, "", nil, fmt.Errorf("session title is required")
	}

	m.mu.Lock()
	if err := m.refreshLocked(); err != nil {
		m.mu.Unlock()
		return nil, "", nil, err
	}
	if instance, rid := m.trackedSessionByIDLocked(stableID); instance != nil {
		m.mu.Unlock()
		return instance, rid, nil, nil
	}
	if repoID != "" {
		key := daemonInstanceKey(repoID, title)
		if instance := m.instances[key]; instance != nil {
			m.mu.Unlock()
			return instance, repoID, nil, nil
		}
	} else {
		// Unscoped: titles are unique per-repo, so collect every match rather
		// than returning the first one the map walk happens to reach. One match
		// resolves; several are ambiguous and must not be silently picked
		// between — a kill/archive would otherwise hit an arbitrary repo's
		// session (the collision resolveActionSession's id-first path avoids).
		var matched *session.Instance
		var matchedRepoID string
		var matchRepoIDs, repoPaths []string
		for key, instance := range m.instances {
			if instance == nil || instance.Title != title {
				continue
			}
			rid, _ := splitDaemonInstanceKey(key)
			if matched == nil {
				matched, matchedRepoID = instance, rid
			}
			matchRepoIDs = append(matchRepoIDs, rid)
			repoPaths = append(repoPaths, instance.Path)
		}
		if len(session.DedupeSorted(matchRepoIDs)) > 1 {
			m.mu.Unlock()
			return nil, "", nil, session.AmbiguousTitleError(title, repoPaths)
		}
		if matched != nil {
			m.mu.Unlock()
			// One live match is NOT proof the title is unique. A second repo's row
			// is skipped during refresh when it cannot be restored (worktree/tmux
			// gone), so it never reaches m.instances — and resolving here would let
			// an unscoped kill/archive hit this repo while the daemon-down disk path
			// would refuse to guess. Union the persisted rows before resolving.
			paths, gaps, err := collectTitleRepoPaths(title)
			if err != nil {
				// Could not enumerate repos at all — strictly LESS evidence than
				// a single unreadable file, so it cannot fail open while that
				// case refuses (#3479). Resolving here is what an unscoped
				// kill/archive would then act on.
				return nil, "", nil, fmt.Errorf("cannot resolve session %q without a project scope: the cross-project ambiguity check could not run, so another project may hold this title too; scope the command with --repo <path>: %w", title, err)
			}
			repos := map[string]string{matchedRepoID: matched.Path}
			for rid, p := range paths {
				repos[rid] = p
			}
			// A collision the readable repos DID show is definitive — no unread
			// file can make a title that is provably held twice unique again —
			// so this answer wins over the gap refusal below.
			if len(repos) > 1 {
				all := make([]string, 0, len(repos))
				for _, p := range repos {
					all = append(all, p)
				}
				return nil, "", nil, session.AmbiguousTitleError(title, all)
			}
			// Nothing readable holds it twice, but the check may not have seen
			// everything — in which case uniqueness is unproven rather than
			// established.
			//
			// A gap in the MATCHED project is not such a case. Ambiguity is
			// defined across distinct repo IDs, and the live match already
			// establishes this project's identity, so its own unreadable file
			// cannot conceal a second project and must not refuse an operation
			// it cannot affect. Same exclusion hookSlugOwnerInOtherRepos makes
			// for its own repo (#3476).
			blocking := make([]config.RepoInstancesSkip, 0, len(gaps))
			for _, gap := range gaps {
				if gap.RepoID == matchedRepoID {
					continue
				}
				blocking = append(blocking, gap)
			}
			if len(blocking) > 0 {
				return nil, "", nil, fmt.Errorf("cannot resolve session %q without a project scope: %s, and any of them may hold this title too; scope the command with --repo <path>, or %s",
					title, config.DescribeRepoInstancesSkips(blocking), config.RepoInstancesSkipRemedy(blocking))
			}
			return matched, matchedRepoID, nil, nil
		}
	}
	m.mu.Unlock()

	data, rid, err := findInstanceDataByTitle(title, repoID)
	if err != nil {
		return nil, "", nil, err
	}
	instance, restoreErr := fromInstanceDataForRefresh(*data, loadRestoreContext(rid, nil))
	if restoreErr != nil {
		return nil, rid, data, nil
	}
	// Give it the daemon-backed credential minter (#3068). A disk-loaded instance
	// has none — FromInstanceData cannot know about the daemon — and without this
	// a restore would provision its replacement sandbox with no callback at all,
	// silently, which is the ordinary case after a daemon restart.
	attachSandboxCredentials(m, instance)

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
			m.warn().Printf("findSession %q: closing duplicate instance attach failed: %v", title, err)
		}
		return tracked, rid, data, nil
	}
	m.instances[key] = instance
	m.mu.Unlock()
	return instance, rid, data, nil
}
