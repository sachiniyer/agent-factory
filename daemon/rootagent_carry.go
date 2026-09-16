package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The reaped root's carry is durable, not only parked in memory (#4400 review
// round 3). reapDeadRoot deletes the session record before the replacement
// exists; an in-memory map alone means a daemon restart inside the
// reap→publish window — or a create that fails and the daemon bounces during
// its backoff — loses the account pin, conversation, tab roster, and notice
// the record was deleted holding, and the next ensure rebuilds the guaranteed
// root on ambient credentials. The carry therefore lands on disk BEFORE the
// record is deleted, survives restarts beside the repo's instances file, and
// is removed by rootEnsureSucceeded once a pass leaves a healthy root (or a
// disable/delete outcome makes it moot).

// reapedRootCarryDisk is the durable form of reapedRootState. Field names are
// the file's schema: they land on disk in every user's AF home, so renaming
// one is a format change, not a refactor.
type reapedRootCarryDisk struct {
	Workspace             string                        `json:"workspace,omitempty"`
	Conversation          session.AgentConversationData `json:"conversation,omitempty"`
	Account               string                        `json:"account,omitempty"`
	Agent                 string                        `json:"agent,omitempty"`
	Tabs                  []session.TabData             `json:"tabs,omitempty"`
	Notice                session.RootRecreateContext   `json:"notice,omitempty"`
	PendingSwap           *session.AccountSwapData      `json:"pending_swap,omitempty"`
	PendingHandoffMission string                        `json:"pending_handoff_mission,omitempty"`
}

func (s reapedRootState) carryDisk() reapedRootCarryDisk {
	return reapedRootCarryDisk{
		Workspace:             s.workspace,
		Conversation:          s.conversation,
		Account:               s.account,
		Agent:                 s.agent,
		Tabs:                  s.tabs,
		Notice:                s.notice,
		PendingSwap:           s.pendingSwap,
		PendingHandoffMission: s.pendingHandoffMission,
	}
}

func (d reapedRootCarryDisk) state() reapedRootState {
	return reapedRootState{
		workspace:             d.Workspace,
		conversation:          d.Conversation,
		account:               d.Account,
		agent:                 d.Agent,
		tabs:                  d.Tabs,
		notice:                d.Notice,
		pendingSwap:           d.PendingSwap,
		pendingHandoffMission: d.PendingHandoffMission,
	}
}

// forWorkspace reports whether this carry may be consumed by a create running
// in workspace. The carry file is keyed by repository ID so every spelling of
// the repository finds it, but the state inside belongs to the checkout that
// parked it; a carry written before the field existed ("" workspace) remains
// consumable anywhere, matching the pre-binding behavior.
func (s reapedRootState) forWorkspace(workspace string) bool {
	return s.workspace == "" ||
		filepath.Clean(s.workspace) == filepath.Clean(workspace)
}

// reapedRootCarryPath places the carry beside the repo's instances.json: one
// per-repo directory holds everything the heal needs to restore, and the repo
// ID validation that file's path resolution already performs covers this one.
func reapedRootCarryPath(repoID string) (string, error) {
	instancesPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(instancesPath), "reaped-root-carry.json"), nil
}

// writeReapedRootCarry durably publishes the carry the reap just snapshot.
// RefusingLink + RemoveFileRefusingLink is the managed-file pair #3672
// established: this file is created and deleted by af alone, so neither end
// acts through a link. Called BEFORE the record delete inside reapDeadRoot —
// after it, a crash between delete and write would lose the carry in exactly
// the window this file exists for.
func (m *Manager) writeReapedRootCarry(repoID string, carried reapedRootState) error {
	path, err := reapedRootCarryPath(repoID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(carried.carryDisk())
	if err != nil {
		return fmt.Errorf("marshal reaped root carry: %w", err)
	}
	return config.AtomicWriteFileRefusingLink(path, data, 0644)
}

// loadReapedRootCarry is the post-restart half of the park: after a daemon
// bounce the in-memory map is empty but the file the reap wrote still names
// what the deleted record carried. present=false means no file exists (the
// ordinary no-carry case), not an empty carry — a reaped root that carried
// nothing still round-trips to present=true with a zero state.
func (m *Manager) loadReapedRootCarry(repoID string) (state reapedRootState, present bool, err error) {
	path, err := reapedRootCarryPath(repoID)
	if err != nil {
		return reapedRootState{}, false, err
	}
	// The write and remove ends already refuse a managed-file symlink; the
	// read end must too. Following the link would let foreign content stand in
	// as the carry af parked — the refusal is about the link's presence, not
	// about what it resolves to, so a dangling link is refused the same way
	// (#4400 review).
	if err := config.RefuseManagedFileSymlink(path); err != nil {
		return reapedRootState{}, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reapedRootState{}, false, nil
		}
		return reapedRootState{}, false, err
	}
	var disk reapedRootCarryDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		return reapedRootState{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return disk.state(), true, nil
}

// ambientSafeCarriedTabs filters a carried roster for a replacement whose
// account pin was rejected: every tmux-backed tab ran its command inside the
// dropped account's environment, so restoring it would relaunch that command
// on ambient credentials. Index 0 (the agent tab) is rebuilt by the launch
// anyway, and web/editor tabs hold no process environment — the only rows
// kept (#4400 review round 4).
//
// Index 0 is kept as a PLACEHOLDER even though the agent row is tmux-backed:
// restoreCarriedTabs and countNonAgentTabs both skip position 0
// unconditionally, so dropping it here would silently consume the first
// surviving web/editor tab (Codex on #4400) — and its Handoffs are the
// account-swap ledger the replacement's fresh agent tab must keep.
func ambientSafeCarriedTabs(tabs []session.TabData) []session.TabData {
	// A fresh backing array, not tabs[:0]: the caller's slice header is a
	// shallow copy of the repo-scoped parked carry's, and compacting in place
	// would overwrite the roster the NEXT create attempt still needs to read
	// after this one fails (Codex on #4400).
	kept := make([]session.TabData, 0, len(tabs))
	for idx, td := range tabs {
		if idx != 0 && td.Kind.HasTmux() {
			continue
		}
		kept = append(kept, td)
	}
	return kept
}

// reconcilePendingSwapConversation keeps the committed swap a replacement
// record carries aligned with the conversation this create is actually
// launching. The carried ConversationID named the committed replacement pane
// on the REAPED root; when the recreate substitutes a newer project
// conversation or falls back to a fresh start, attaching the obsolete id
// makes the settlement sync stamp it over the live agent's — or reject the
// live conversation — on every tick (Codex on #4400). The swap is cloned, not
// mutated: pendingAccountSwap is a pointer shared with the repo-scoped parked
// carry, and writing through it would corrupt the state a later retry reads.
func reconcilePendingSwapConversation(req *CreateSessionRequest) {
	swap := req.pendingAccountSwap
	if swap == nil || swap.ConversationID == "" {
		return
	}
	reconciled := *swap
	if req.resumeConversation.Agent == tmux.ProgramClaude && req.resumeConversation.HasID() {
		reconciled.ConversationID = req.resumeConversation.ID
	} else {
		reconciled.ConversationID = ""
	}
	req.pendingAccountSwap = &reconciled
}

// retireReapedRootCarry drops both halves of the parked carry. The create
// goroutine calls it after a successful publish: rootEnsureSucceeded leaves
// the carry alone while this repo's in-flight mark is held — the mark only
// clears in the deferred finishRootCreate AFTER runRootCreate returns — so
// the create that owns the mark retires the carry itself once its outcome is
// known (#4400 review round 4).
func (m *Manager) retireReapedRootCarry(repoID string) {
	m.mu.Lock()
	delete(m.reapedRootCarries, repoID)
	m.mu.Unlock()
	m.removeReapedRootCarry(repoID)
}

// removeReapedRootCarry drops the parked carry once a pass makes it moot.
// A missing file is the common case (nothing was ever reaped); a remove
// failure only leaves a stale file the next reap overwrites — a warning, not
// a heal blocker, so callers log rather than propagate it.
func (m *Manager) removeReapedRootCarry(repoID string) {
	path, err := reapedRootCarryPath(repoID)
	if err != nil {
		m.warn().Printf("could not resolve the reaped root carry path for repo %s: %v", repoID, err)
		return
	}
	if err := config.RemoveFileRefusingLink(path); err != nil && !os.IsNotExist(err) {
		m.warn().Printf("could not remove the parked reaped root carry for repo %s: %v", repoID, err)
	}
}
