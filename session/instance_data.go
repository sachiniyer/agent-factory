package session

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// ToInstanceData converts an Instance to its serializable form
func (i *Instance) ToInstanceData() InstanceData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.toInstanceDataLocked()
}

// ToInstanceDataWithEpoch returns the serializable form together with the state
// epoch it was read at, both under ONE hold of i.mu (#2135). A writer that
// persists what it read uses it so the epoch it re-checks before the write
// provably belongs to the payload it is about to write — reading the two
// separately would leave a window in which the state moves between them, which is
// the very thing the epoch exists to detect.
func (i *Instance) ToInstanceDataWithEpoch() (InstanceData, uint64) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.toInstanceDataLocked(), i.stateEpoch
}

// toInstanceDataLocked is the shared body. Caller holds i.mu (read or write).
func (i *Instance) toInstanceDataLocked() InstanceData {
	data := InstanceData{
		ID:     i.ID,
		TaskID: i.TaskID,
		Title:  i.Title,
		Path:   i.Path,
		Branch: i.Branch,
		// Serialize the two-axis state plus the legacy composed Status. Liveness is
		// the daemon truth; InFlightOp rides daemon snapshots so secondary TUIs can
		// cold-start into archive/restore operations without lossy Status
		// reconstruction (#1436). Disk writers scrub InFlightOp before persistence.
		Status:     i.statusLocked(),
		Liveness:   i.liveness,
		InFlightOp: i.inFlightOp,
		Height:     i.Height,
		Width:      i.Width,
		CreatedAt:  i.CreatedAt,
		UpdatedAt:  time.Now(),
		Program:    i.Program,
		AutoYes:    i.AutoYes,
		Prompt:     i.Prompt,
		UserKilled: i.userKilled,
	}

	if i.backend != nil {
		data.BackendType = i.backend.Type()
	}

	// Persist the usage-limit reset time only while the session is actually
	// limit-blocked (#1146). Gating on the liveness keeps a recovered session
	// from carrying a stale reset time to disk or into the snapshot — the
	// in-memory field lingers after ClearLimitReached but is never serialized.
	if i.liveness == LiveLimitReached {
		data.LimitResetAt = i.limitResetAt
	}

	// Unlike the reset time above, this is NOT gated on a liveness: whether the run
	// is still in flight is meaningful in every state (#1892), and gating it would
	// reintroduce the bug it fixes — a session whose run is live must read as active
	// whether it is Running, limit-parked, mid-archive, or Lost.
	data.TaskRunActive = i.taskRunActive

	// Persist each tab so the full agent+shell tab list survives a restart
	// (Sachin's hard requirement for #930): on reload FromInstanceData restores
	// each local tab's tmux session by its exact persisted name, reconnecting
	// live sessions across an af/daemon restart. Remote tabs (agent + optional
	// terminal) carry no tmux session, so they serialize with an empty TmuxName;
	// on restore HookBackend.Start re-derives them from the live terminal_cmd
	// config (syncRemoteTabs) rather than from this serialized list, so a
	// terminal_cmd added or removed while af was down is honored.
	for _, tab := range i.Tabs {
		td := TabData{ID: tab.ID, Name: tab.Name, Kind: tab.Kind, Command: tab.Command, URL: tab.URL}
		if tab.tmux != nil {
			td.TmuxName = tab.tmux.SanitizedName()
		}
		td.Conversation = conversationDataPtr(tab.Conversation)
		if len(tab.Handoffs) > 0 {
			td.Handoffs = append([]AgentHandoff(nil), tab.Handoffs...)
		}
		data.Tabs = append(data.Tabs, td)
	}

	// Keep writing the legacy single TmuxName field (set from the agent tab) for
	// one release: a binary rolled back to before #930 PR 2 still finds the
	// agent session by its exact name, and old readers ignore the new Tabs list.
	if ts := i.tmuxLocked(); ts != nil {
		data.TmuxName = ts.SanitizedName()
	}
	if len(i.Tabs) > 0 {
		data.AgentConversation = conversationDataPtr(i.Tabs[0].Conversation)
	}

	// Only include worktree data if gitWorktree is initialized
	if i.gitWorktree != nil {
		branchCreatedByUs := i.gitWorktree.BranchCreatedByUs()
		// ExternalWorktree is true for in-place sessions (`af sessions create
		// --here`, which attach to the repo's own working tree) and for
		// instances persisted by the pre-#930-PR-3 create-on-existing-worktree
		// feature. Cleanup() honors it by skipping removal of the user-owned
		// worktree+branch. (BranchCreatedByUs is independent — it also flips
		// false on the normal path when Setup reuses an existing branch; see
		// git/worktree_ops.go setupFromExistingBranch.)
		data.Worktree = GitWorktreeData{
			RepoPath:          i.gitWorktree.GetRepoPath(),
			WorktreePath:      i.gitWorktree.GetWorktreePath(),
			SessionName:       i.Title,
			BranchName:        i.gitWorktree.GetBranchName(),
			BaseCommitSHA:     i.gitWorktree.GetBaseCommitSHA(),
			ExternalWorktree:  i.gitWorktree.IsExternalWorktree(),
			BranchCreatedByUs: &branchCreatedByUs,
		}
	}

	// Only include PR info if it exists
	if i.prInfo != nil {
		data.PRInfo = PRInfoData{
			Number: i.prInfo.Number,
			Title:  i.prInfo.Title,
			URL:    i.prInfo.URL,
			State:  i.prInfo.State,
		}
	}

	return data
}

// FromInstanceData creates a new Instance from serialized data
func FromInstanceData(data InstanceData) (*Instance, error) {
	// Resolve the liveness axis via the shared rollforward (#1108/#1195): prefer
	// the new `liveness` field, fall back to the legacy `status` int for records
	// written before #1195, and load a persisted Dead as Lost — recovery-
	// eligible, which is what makes sessions stranded by an outage under an old
	// build restorable after an upgrade. A tombstoned record keeps its (Lost)
	// status; the daemon finishes its teardown rather than restoring it.
	liveness := livenessFromData(data)
	// Resolve the in-flight-op axis from the snapshot payload, falling back to
	// the legacy status for old daemons/records. A persisted record is always
	// settled (disk writers scrub this field and SaveInstances skips
	// Loading/Deleting), but a SNAPSHOT can catch archive/restore in progress and
	// must round-trip the exact op (#1436).
	inFlightOp := inFlightOpFromData(data)
	instance := &Instance{
		ID:         data.ID,
		TaskID:     data.TaskID,
		Title:      data.Title,
		Path:       data.Path,
		Branch:     data.Branch,
		liveness:   liveness,
		inFlightOp: inFlightOp,
		// Carried across the restart (#1892). An outage that loses sessions is the
		// same event that restarts the daemon, so this fact has to come back from
		// disk or the cap would re-decide it from a Lost state that cannot tell a
		// finished run from an interrupted one.
		taskRunActive: data.TaskRunActive,
		limitResetAt:  data.LimitResetAt,
		Height:        data.Height,
		Width:         data.Width,
		CreatedAt:     data.CreatedAt,
		UpdatedAt:     data.UpdatedAt,
		Program:       data.Program,
		AutoYes:       data.AutoYes,
		Prompt:        data.Prompt,
		userKilled:    data.UserKilled,
	}

	// Pick backend based on persisted BackendType.
	switch {
	case isSandboxBackendType(data.BackendType):
		// A sandbox session (docker/ssh/hook, #1592 Phase 4 PR6/PR7) has no
		// daemon-side worktree or tmux to reconstruct — its workspace lived in a
		// container/remote/user-provisioned sandbox that does not survive a daemon
		// restart; only its pushed branch on origin does. Rebuild only an INERT
		// sandbox backend so the row still classifies as its runtime
		// (Type/Capabilities) for archive + restore; the live runtime is
		// re-provisioned on restore (re-running launch_cmd for hook), never
		// reconstructed here.
		instance.backend = newInertSandboxBackend(data.BackendType)
	default:
		instance.backend = &LocalBackend{}

		// DESTRUCTION REQUIRES POSITIVE EVIDENCE (#1953). A missing
		// branch_created_by_us (written before the field landed 2026-04-17)
		// means we do not know who created the branch — and the only thing
		// this flag authorizes is `git branch -D`. Unknown provenance
		// therefore means KEEP: default to false.
		//
		// This inverts the original nil→true back-compat default, which
		// assumed every legacy record was a branch AF had created. It was
		// wrong for two legacy shapes that both persisted no flag:
		// attach-to-existing-worktree records (external_worktree=true,
		// 2026-03-03) and — with nothing external about them — any normal
		// linked worktree Setup built on a branch the user already had
		// (setupFromExistingBranch, 2025-07-23). Both are the user's
		// branches; the old default marked them AF-created and let reset and
		// kill/archive delete them.
		//
		// The cost is the opposite error: a legacy record whose branch AF
		// really did create is now never pruned, leaking an orphaned af-*
		// branch. That is deliberate — a leaked branch the user can see and
		// delete beats a deletion they cannot undo.
		branchCreatedByUs := false
		if data.Worktree.BranchCreatedByUs != nil {
			branchCreatedByUs = *data.Worktree.BranchCreatedByUs
		}

		gw, err := git.NewGitWorktreeFromStorage(
			data.Worktree.RepoPath,
			data.Worktree.WorktreePath,
			data.Worktree.SessionName,
			data.Worktree.BranchName,
			data.Worktree.BaseCommitSHA,
			data.Worktree.ExternalWorktree,
			branchCreatedByUs,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to restore git worktree: %w", err)
		}
		instance.gitWorktree = gw

		// Rebuild the instance's tab list from disk so every tab (agent + shell)
		// reconnects to its exact tmux session across an af/daemon restart — the
		// load-bearing #930 requirement. LocalBackend.Start(false) then restores
		// each tab's session.
		restoreLocalTabs(instance, data)
	}

	if data.PRInfo.Number != 0 {
		instance.prInfo = &git.PRInfo{
			Number: data.PRInfo.Number,
			Title:  data.PRInfo.Title,
			URL:    data.PRInfo.URL,
			State:  data.PRInfo.State,
		}
	}

	// An archived session (#1028) loads INERT: its tmux was torn down and its
	// worktree moved to the global archive dir at archive time, so there is
	// nothing to re-spawn or reconnect. Skipping Start leaves started=false and
	// no tmux binding, which is exactly what makes the status poll (skips
	// !Started), the Lost-restore loop (gates on ==Lost), and EnsureRootAgents
	// pass it by — the session sits quiescent until an explicit RestoreArchived.
	// This is also #970-consistent: a load must never itself un-archive a
	// session (no worktree move, no spawn) as a side effect. gitWorktree is
	// already bound above to the persisted (archived) path so restore knows
	// where the worktree currently lives; the Tabs list restored above is
	// tmux-less for the same reason (its TmuxName entries reference sessions
	// that no longer exist, and restoreLocalTabs only binds names, never spawns).
	if liveness == LiveArchived {
		return instance, nil
	}

	// A non-archived sandbox session (docker/ssh) cannot be driven on load: its
	// container/remote is gone after a daemon restart and only its pushed branch
	// on origin survives (#1592 Phase 4 PR6). Load it INERT + Lost — started stays
	// false, so the status poll and the Lost-restore loop both pass it by (the
	// #1028 started=false fence) — and let an explicit restore re-provision a fresh
	// sandbox from the branch. Skipping Start also avoids driving the recursive
	// remote backend with no live agent-server client.
	if isSandboxBackendType(data.BackendType) {
		instance.liveness = LiveLost
		return instance, nil
	}

	if err := instance.Start(false); err != nil {
		return nil, err
	}

	return instance, nil
}

// restoreTmuxSession constructs a tmux session for an exact persisted name. It
// is a package var (not a direct call) so restore-survival tests can inject
// mock-backed sessions and stay hermetic; production uses the real constructor.
var restoreTmuxSession = tmux.NewTmuxSessionFromSanitizedName

// restoreLocalTabs rebuilds a local instance's tab list from persisted data.
//
//   - New format (data.Tabs present): each tab is reconstructed in order, and
//     any tab with a persisted tmux name is bound to that exact session so
//     LocalBackend.Start can reconnect it across a restart.
//   - Legacy format (no data.Tabs, written before #930 PR 2): synthesize the
//     single Agent tab from the legacy TmuxName/Program — keeping the EXACT
//     legacy tmux name so an existing live agent session survives the upgrade.
//     No shell tab is synthesized: terminal tabs are on-demand only (#1100).
func restoreLocalTabs(instance *Instance, data InstanceData) {
	if len(data.Tabs) > 0 {
		for idx, td := range data.Tabs {
			kind := tabKindForData(td.Kind)
			var ts *tmux.TmuxSession
			if td.TmuxName != "" {
				ts = restoreTmuxSession(td.TmuxName, tabProgram(kind, td.Command, data.Program))
			}
			var conversation AgentConversationData
			if td.Conversation != nil {
				conversation = *td.Conversation
			} else if idx == 0 && data.AgentConversation != nil {
				conversation = *data.AgentConversation
			}
			// Backfill a stable id for a legacy tab persisted before #1738 (no id):
			// mint one now so the restored tab is addressable by a stable id from
			// this load forward (rollforward, mirroring the InstanceData.ID backfill).
			id := td.ID
			if id == "" {
				id = newTabID()
			}
			var handoffs []AgentHandoff
			if len(td.Handoffs) > 0 {
				handoffs = append([]AgentHandoff(nil), td.Handoffs...)
			}
			instance.Tabs = append(instance.Tabs, &Tab{
				ID:           id,
				Name:         td.Name,
				Kind:         kind,
				Command:      td.Command,
				URL:          td.URL,
				Conversation: conversation,
				Handoffs:     handoffs,
				tmux:         ts,
			})
		}
		return
	}

	// Legacy single-session format: the agent tab keeps its exact legacy name.
	if data.TmuxName != "" {
		instance.setTmuxLocked(restoreTmuxSession(data.TmuxName, data.Program))
	} else {
		instance.setTmuxLocked(tmux.NewTmuxSession(data.Title, data.Program))
	}
	if data.AgentConversation != nil {
		instance.SetAgentConversation(*data.AgentConversation)
	}
}
