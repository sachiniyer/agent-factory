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
// The name lives in config because `af reset` clears it with the records.
func reapedRootCarryPath(repoID string) (string, error) {
	return config.RepoReapedRootCarryPath(repoID)
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
// nothing still round-trips to present=true with a zero state. A non-nil err
// means a carry may exist that could not be read; it is never "no carry", and
// the consumer fails its ensure on it rather than rebuild without it.
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

// reapedRootCarryUnreadableError words a present-but-unreadable carry as the
// ensure failure it is: what is being withheld, which file, and the two ways
// out — repair the file so the next retry restores it, or remove it to accept
// the ambient identity.
func reapedRootCarryUnreadableError(repoID string, loadErr error) error {
	path, pathErr := reapedRootCarryPath(repoID)
	if pathErr != nil {
		path = "reaped-root-carry.json for repo " + repoID
	}
	return fmt.Errorf("not re-creating the root agent: the carry its reaped predecessor parked (account pin, conversation, tabs, pending account swap) could not be read, and an ambient rebuild would retire it — fix %s so the next retry can restore it, or remove it to start the root on the ambient identity: %w",
		path, loadErr)
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

// rootHealAbandonedCarryReason is what a replacement is told when the heal
// could not bring the carried conversation back. Worded like session's own
// staleCarryReason and abandonedCarryReason, because the same notice repeats it.
const rootHealAbandonedCarryReason = "the root agent's tmux vanished and its replacement could not resume the carried conversation"

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
	if swap == nil {
		return
	}
	// A CARRIED conversation is the swap's other shape (#4367/#4504, merged
	// into this branch from master): a same-agent swap copies the outgoing
	// conversation into the incoming account's home and resumes it, recording
	// CarriedConversationID instead of ConversationID — at most one of the two
	// is ever set. A root account handoff is same-agent by construction, so
	// this is now the SHAPE THIS PR'S FEATURE PRODUCES, not an edge case.
	//
	// It needs its own reconciliation because the settlement validates it
	// harder than the injected id: synchronizeCarriedConversationLocked
	// REJECTS a live agent recording any other conversation, and stamps the
	// carried id onto an empty slot. So a replacement that could not resume
	// the carried conversation — its copy is gone, or the resume failed and
	// the create fell back to a fresh agent — would either be fenced behind a
	// swap that can never settle, or have its record claim a conversation it
	// is not running. session's own give-up path (demotePendingAccountSwapCarry)
	// cannot reach this: it rebuilds an accountSwapLaunch plan, which a
	// CreateSession replacement never has.
	//
	// So the heal applies that path's rule itself: keep the carry when this
	// launch really is resuming it, and otherwise demote it to a fresh start
	// that says why — which is also what planAccountSwapCarry reads on a later
	// respawn (a committed swap with no carried id takes CarryFallback and does
	// not try to carry again).
	if swap.CarriedConversationID != "" {
		if req.resumeConversation.HasID() && req.resumeConversation.ID == swap.CarriedConversationID {
			return
		}
		reconciled := *swap
		reconciled.CarriedConversationID = ""
		reconciled.CarrySourceAccount = ""
		reconciled.CarriedLaunchStarted = false
		reconciled.CarryFallback = rootHealAbandonedCarryReason
		// Left empty rather than pointed at whatever this create launched:
		// ConversationID is documented as a freshly INJECTED id, and a respawn
		// re-injects it with --session-id, which would fork a conversation that
		// already exists.
		reconciled.ConversationID = ""
		req.pendingAccountSwap = &reconciled
		return
	}
	if swap.ConversationID == "" {
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

// retireReapedRootCarry drops the parked carry once a pass has made it moot —
// but only a carry that pass can speak for (#4400 review round 7). The carry is
// keyed by repository and bound to the checkout that parked it, so one retire
// keyed by repository alone let a create in linked worktree B delete worktree
// A's account pin, pending swap, and takeover brief without a log line.
//
// consumed says the caller's own create restored this carry: the create goroutine
// calls with true after a successful publish, because rootEnsureSucceeded leaves
// the carry alone while this repo's in-flight mark is held (#4400 review round
// 4). Otherwise workspace names the checkout whose healthy root the pass just
// established, and only a carry bound to it — or one written before binding
// existed — is moot:
//
//	parked carry           | action
//	-----------------------+-------------------------------------------------
//	none                   | nothing
//	bound to workspace     | retire the map entry and the file
//	bound to another       | keep it parked for that checkout's entry; warn once
//	present but unreadable | keep it; warn once (its binding is unknown)
//
// Keeping an unreadable carry is the read policy the no-record create already
// applies (#4400 review round 6): the file may be the only copy of an account
// pin, and a pass that cannot read it cannot tell whose it is. The warning names
// the file, so removing it stays the operator's call.
func (m *Manager) retireReapedRootCarry(repoID, workspace string, consumed bool) {
	if !consumed {
		parked, present, err := m.parkedReapedRootCarry(repoID)
		switch {
		case err != nil:
			m.warnReapedRootCarryOnce(repoID, reapedRootCarryUnreadableNotice(repoID, err))
			return
		case !present:
			m.clearReapedRootCarryNotice(repoID)
			return
		case !parked.forWorkspace(workspace):
			m.warnReapedRootCarryOnce(repoID, fmt.Sprintf(
				"leaving the root agent carry reaped in %s parked for that checkout's own root_agents entry while the root runs in %s (account %q, pending account swap: %t)",
				parked.workspace, workspace, parked.account, parked.pendingSwap != nil))
			return
		}
	}
	m.mu.Lock()
	delete(m.reapedRootCarries, repoID)
	m.mu.Unlock()
	if _, clear := m.removeReapedRootCarry(repoID); clear {
		m.clearReapedRootCarryNotice(repoID)
	}
}

// discardReapedRootCarry retires the parked carry whatever it is bound to, for
// the two outcomes that leave it no consumer at all: the project was deleted,
// or no enabled root_agents spelling of the repository remains. It says what it
// discarded, because the carry can hold an account pin and a pending swap.
func (m *Manager) discardReapedRootCarry(repoID, reason string) {
	parked, present, loadErr := m.parkedReapedRootCarry(repoID)
	m.mu.Lock()
	delete(m.reapedRootCarries, repoID)
	m.mu.Unlock()
	existed, clear := m.removeReapedRootCarry(repoID)
	if !clear {
		return
	}
	m.clearReapedRootCarryNotice(repoID)
	switch {
	case present:
		m.warn().Printf("discarded the root agent carry reaped in %s (account %q, pending account swap: %t): %s",
			parked.workspace, parked.account, parked.pendingSwap != nil, reason)
	case existed || loadErr != nil:
		m.warn().Printf("discarded the unreadable parked root agent carry for repo %s: %s", repoID, reason)
	}
}

// parkedReapedRootCarry returns the carry parked for repoID: the in-memory park
// when there is one, otherwise the durable file, hydrated into the map so a
// carry left for another checkout costs one read rather than one per tick.
// After a restart the file outlives the map, which is why a healthy pass has to
// read it at all before deciding it is moot.
func (m *Manager) parkedReapedRootCarry(repoID string) (reapedRootState, bool, error) {
	m.mu.Lock()
	parked, ok := m.reapedRootCarries[repoID]
	m.mu.Unlock()
	if ok {
		return parked, true, nil
	}
	parked, ok, err := m.loadReapedRootCarry(repoID)
	if err != nil || !ok {
		return reapedRootState{}, false, err
	}
	m.mu.Lock()
	if current, raced := m.reapedRootCarries[repoID]; raced {
		parked = current
	} else {
		m.reapedRootCarries[repoID] = parked
	}
	m.mu.Unlock()
	return parked, true, nil
}

// reapedRootCarryUnreadableNotice words a carry a healthy pass could not read.
func reapedRootCarryUnreadableNotice(repoID string, err error) string {
	path, pathErr := reapedRootCarryPath(repoID)
	if pathErr != nil {
		path = "reaped-root-carry.json for repo " + repoID
	}
	return fmt.Sprintf("leaving the parked root agent carry at %s in place: it could not be read, so which checkout it belongs to is unknown — remove it once the root it was reaped from no longer needs its account pin or pending account swap: %v",
		path, err)
}

// removeReapedRootCarry unlinks the durable carry. existed reports that a file
// was removed; clear reports that the path holds no carry now. A missing file is
// the common case (nothing was ever reaped). A failure leaves a stale file the
// next reap overwrites — a warning, not a heal blocker — and it is logged once
// per distinct cause: a symlink or directory at the path fails the same way on
// every healthy tick (#4400 review round 7).
func (m *Manager) removeReapedRootCarry(repoID string) (existed, clear bool) {
	path, err := reapedRootCarryPath(repoID)
	if err != nil {
		m.warnReapedRootCarryOnce(repoID, fmt.Sprintf("could not resolve the reaped root carry path for repo %s: %v", repoID, err))
		return false, false
	}
	err = config.RemoveFileRefusingLink(path)
	switch {
	case err == nil:
		return true, true
	case os.IsNotExist(err):
		return false, true
	default:
		m.warnReapedRootCarryOnce(repoID, fmt.Sprintf("could not remove the parked reaped root carry for repo %s: %v", repoID, err))
		return false, false
	}
}

// warnReapedRootCarryOnce logs a carry warning unless it is the one already
// logged for this repo. Every carry decision a healthy root re-runs on the
// one-second ensure cadence goes through here, so a condition that persists is
// reported when it starts and again only when it changes.
func (m *Manager) warnReapedRootCarryOnce(repoID, message string) {
	m.mu.Lock()
	if m.reapedRootCarryNotices[repoID] == message {
		m.mu.Unlock()
		return
	}
	if m.reapedRootCarryNotices == nil {
		m.reapedRootCarryNotices = make(map[string]string)
	}
	m.reapedRootCarryNotices[repoID] = message
	m.mu.Unlock()
	m.warn().Print(message)
}

// clearReapedRootCarryNotice re-arms warnReapedRootCarryOnce once the condition
// it reported has cleared.
func (m *Manager) clearReapedRootCarryNotice(repoID string) {
	m.mu.Lock()
	delete(m.reapedRootCarryNotices, repoID)
	m.mu.Unlock()
}
