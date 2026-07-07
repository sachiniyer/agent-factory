package session

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

type Status int

const (
	// Running is the status when the instance is running and claude is working.
	Running Status = iota
	// Ready is if the claude instance is ready to be interacted with (waiting for user input).
	Ready
	// Loading is if the instance is loading (if we are setting it up).
	Loading
	// Deleting is if the instance is being torn down asynchronously after the
	// user confirmed a kill. Like Loading it is transient in-memory state: it
	// is never persisted (SaveInstances skips Loading/Deleting) and the row is
	// removed or reverted when the background teardown finishes (#844).
	Deleting
	// Dead is when the underlying tmux/remote session has vanished out from
	// under us (e.g. the tmux server was killed externally). The row is a
	// corpse: the user can no longer attach to it (handleEnter surfaces an
	// error instead of silently swallowing Enter) but can still kill it. A
	// dead session's HasUpdated latches (false,false) — the same value a
	// healthy idle session returns — so without an explicit liveness probe the
	// metadata tick would repaint it Ready (green dot) forever, making a corpse
	// masquerade as healthy (#935). Unlike Loading/Deleting this is NOT
	// in-flight TUI state: it is persisted and background syncs may still reap
	// or replace the row, so it is deliberately excluded from isTransientStatus.
	//
	// As of #1108 Dead is write-never: observed disappearance is recorded as
	// Lost instead, and FromInstanceData rewrites persisted Dead to Lost on
	// load (rollforward — the only writers of persisted Dead were
	// observed-death paths; user kills delete the record). The value stays in
	// the enum because Status serializes as an int: appending, never
	// renumbering, is what keeps old records readable.
	Dead
	// Lost is when the underlying tmux/remote session vanished out from under
	// a live session with no user kill on record — the tmux server was killed,
	// an outage/OOM starved it (#1104), or the box rebooted while the daemon
	// had already observed the death. Unlike a user-killed session (whose
	// record is deleted, with a UserKilled tombstone covering the teardown
	// crash window), a Lost session is wanted: it is recovery-eligible and the
	// daemon restores it best-effort (#1108). Persisted, like Dead; excluded
	// from isTransientStatus for the same reason.
	Lost
	// Archived is the deliberate counterpart of Lost (#1028): the user ran
	// `af sessions archive`, so the daemon tore down every tmux session (agent
	// + shell/process tabs) and MOVED the worktree out to the global archive
	// dir (<AGENT_FACTORY_HOME>/archived/<repoID>/<title>/). Where Lost is a
	// wanted, actively-self-healing state (tmux vanished under a live record;
	// the restore loop re-spawns it every poll), Archived is a wanted,
	// QUIESCENT state: it is never probed, never marked Lost, and never
	// auto-restored — only an explicit `af sessions restore` moves the worktree
	// back and re-spawns the agent. It therefore loads INERT (FromInstanceData
	// skips Start: no tmux binding, started=false), which is what keeps the
	// status poll (skips !Started), the Lost-restore loop (gates on ==Lost),
	// and the root ensure loop from touching it. Persisted, like Dead/Lost;
	// appended, never renumbered (Status serializes as an int), so old records
	// stay readable — the same rollforward discipline #658/#1108 rely on.
	Archived
)

// Instance is a running instance of claude code.
type Instance struct {
	// mu protects fields that are accessed concurrently by async Start()
	// goroutines (writers) and the main bubbletea loop (readers):
	// started, liveness/inFlightOp, Tabs (and the agent tab's tmux session),
	// gitWorktree, prInfo, diffStats.
	mu sync.RWMutex

	// ID is the instance's stable identity (#1195): a random UUID minted once at
	// NewInstance, persisted, and never mutated. The reconcile uses it to tell
	// "same session" from "title reused" (#765) without leaning on CreatedAt
	// equality — the audit's identity-by-circumstance gotcha (a manufactured or
	// zero-CreatedAt record silently degraded a swap into an in-place corpse
	// mutation). Legacy records persisted before #1195 carry no ID; the reconcile
	// falls back to title+CreatedAt for them until they are recreated. Immutable
	// after construction, so cross-goroutine readers may read it without the mutex
	// (like Title).
	ID string
	// Title is the title of the instance.
	Title string
	// Path is the path to the workspace.
	Path string
	// Branch is the branch of the instance.
	Branch string
	// liveness and inFlightOp are the two orthogonal axes of session state
	// (#1195): liveness is the daemon-owned health of the backing session
	// (Running/Ready/Lost/Archived/…), inFlightOp is the transient, never-
	// persisted client operation overlaid on it (Creating/Killing/Archiving/
	// Restoring). The legacy Status enum is derived from them via the
	// GetStatus/SetStatus shim in liveness.go. Both are mutex-protected.
	liveness   Liveness
	inFlightOp InFlightOp
	// limitResetAt is the parsed usage-limit reset time (#1146), display-only in
	// PR2: set alongside liveness == LiveLimitReached when the pane shows a limit
	// banner carrying a parseable reset time (zero when it carried none). Read
	// only while limit-blocked (LimitResetAt/ToInstanceData gate on the liveness),
	// so a lingering value on a recovered session never surfaces. Persisted and
	// carried in the daemon snapshot so the badge survives a restart; PR3's
	// auto-resume scheduler reads it. Mutex-protected.
	limitResetAt time.Time
	// Program is the program to run in the instance.
	Program string
	// Height is the height of the instance.
	Height int
	// Width is the width of the instance.
	Width int
	// CreatedAt is the time the instance was created.
	CreatedAt time.Time
	// UpdatedAt is the time the instance was last updated.
	UpdatedAt time.Time
	// AutoYes is true if the instance should automatically press enter when prompted.
	AutoYes bool
	// Prompt is the initial prompt to pass to the instance on startup
	Prompt string
	// inPlace is true when the instance was created with `--here`: on first
	// start it attaches to the repo's existing working tree at its current
	// branch (external worktree) instead of creating a fresh worktree+branch.
	// Set once by NewInstance and only read by LocalBackend.Start's first-time
	// setup; restored instances don't need it (the persisted
	// external_worktree flag carries the semantics from then on).
	inPlace bool

	// userKilled is the kill-intent tombstone (#1108): set (and persisted)
	// before an explicit kill's teardown begins, so a record that survives a
	// daemon crash or teardown failure mid-kill is provably a corpse the user
	// wanted dead — never classified Lost, never restored. The happy kill path
	// deletes the record entirely, so the tombstone is normally invisible; the
	// daemon poll finishes a tombstoned record's teardown instead of probing it.
	userKilled bool

	// prInfo stores the associated GitHub PR info
	prInfo *git.PRInfo
	// prInfoLastFetched is the wall-clock time of the most recent PR info
	// fetch. Not persisted — restored instances start with a zero value so
	// the first lazy fetch on selection always runs. Used to debounce
	// repeated fetches when the user cycles the sidebar.
	prInfoLastFetched time.Time

	// backend abstracts session lifecycle (local tmux+git vs remote hooks).
	backend Backend
	// remoteMeta stores additional metadata returned by remote hook scripts.
	remoteMeta map[string]interface{}

	// The below fields are initialized upon calling Start().

	started bool
	// Tabs is the instance's ordered list of tabs. In PR 1 of the #930
	// ephemeral-tabs epic this holds exactly one Agent-kind tab (Tabs[0]) that
	// wraps the instance's single tmux session; every tmux-touching method
	// routes through it via tmuxLocked/setTmuxLocked. Remote/hook-backed
	// instances drive their agent session through hook commands and so carry no
	// tmux-backed tab. Later PRs add shell/process tabs, lifecycle, and per-tab
	// persistence.
	Tabs []*Tab
	// gitWorktree is the git worktree for the instance.
	gitWorktree *git.GitWorktree
}

// tmuxLocked returns the agent tab's tmux session, or nil when the instance has
// no agent tab yet (not started) or is remote. Callers must hold i.mu (read or
// write).
func (i *Instance) tmuxLocked() *tmux.TmuxSession {
	if len(i.Tabs) == 0 {
		return nil
	}
	return i.Tabs[0].tmux
}

// shellTabLocked returns the instance's Shell-kind tab (the terminal tab), or
// nil if it has none yet. Callers must hold i.mu (read or write).
func (i *Instance) shellTabLocked() *Tab {
	for _, t := range i.Tabs {
		if t.Kind == TabKindShell {
			return t
		}
	}
	return nil
}

// GetTabs returns a snapshot of the instance's tab list under the instance
// mutex. The returned slice is a copy so callers (the UI tab bar) can iterate
// without racing concurrent tab mutation; the *Tab elements' Name/Kind are set
// once at creation and never mutated, so they are safe to read.
func (i *Instance) GetTabs() []*Tab {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]*Tab, len(i.Tabs))
	copy(out, i.Tabs)
	return out
}

// tabTmuxAtLocked returns the tmux session of the tab at idx, or nil when idx
// is out of range or the tab has no session. Callers must hold i.mu.
func (i *Instance) tabTmuxAtLocked(idx int) *tmux.TmuxSession {
	if idx < 0 || idx >= len(i.Tabs) {
		return nil
	}
	return i.Tabs[idx].tmux
}

// PreviewTab captures the detached content of the tab at idx. Returns ("", nil)
// when the instance is not started or the tab has no live session, and
// tmux.ErrSessionGone when the session vanished — mirroring Instance.Preview for
// the agent tab so the UI can degrade gracefully. idx is the same 0-based index
// the UI tab bar uses (0 is the agent tab; 1+ are shell/process tabs).
func (i *Instance) PreviewTab(idx int) (string, error) {
	i.mu.RLock()
	started := i.started
	ts := i.tabTmuxAtLocked(idx)
	i.mu.RUnlock()
	if !started || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContent()
}

// PreviewTabFullHistory captures the full scrollback of the tab at idx, used
// when entering scroll mode. Same nil/started guards as PreviewTab.
func (i *Instance) PreviewTabFullHistory(idx int) (string, error) {
	i.mu.RLock()
	started := i.started
	ts := i.tabTmuxAtLocked(idx)
	i.mu.RUnlock()
	if !started || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContentWithOptions("-", "-")
}

// TabAlive reports whether the tab at idx has a live tmux session.
func (i *Instance) TabAlive(idx int) bool {
	i.mu.RLock()
	ts := i.tabTmuxAtLocked(idx)
	i.mu.RUnlock()
	return ts != nil && ts.DoesSessionExist()
}

// AttachTab attaches (interactive PTY) to the tab at idx. The captured-instance
// semantics that protect deferred attach flows from selection drift (#716) are
// inherent here: the tab's session belongs to this instance, so there is no
// title-keyed cache to drift.
func (i *Instance) AttachTab(idx int) (chan struct{}, error) {
	i.mu.RLock()
	ts := i.tabTmuxAtLocked(idx)
	i.mu.RUnlock()
	if ts == nil {
		return nil, fmt.Errorf("no terminal session to attach to")
	}
	if !ts.DoesSessionExist() {
		return nil, fmt.Errorf("terminal session does not exist")
	}
	return ts.Attach()
}

// SetTabDetachedSize resizes the detached session of the tab at idx so its
// capture matches the pane dimensions. A no-op when the tab has no live session.
func (i *Instance) SetTabDetachedSize(idx, width, height int) error {
	i.mu.RLock()
	ts := i.tabTmuxAtLocked(idx)
	i.mu.RUnlock()
	if ts == nil {
		return nil
	}
	return ts.SetDetachedSize(width, height)
}

// TabCount returns the number of tabs the instance currently holds.
func (i *Instance) TabCount() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.Tabs)
}

// TabTmuxName returns the sanitized tmux session name of the tab at idx, or
// "" when the instance is not started or the tab has no local session
// (remote tabs, out-of-range idx). The embedded terminal pane (#1089) uses it
// to attach its own render client to the tab's session; it never creates or
// mutates the session.
func (i *Instance) TabTmuxName(idx int) string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.started {
		return ""
	}
	ts := i.tabTmuxAtLocked(idx)
	if ts == nil {
		return ""
	}
	return ts.SanitizedName()
}

// CloseTab kills the tab at idx and removes it from Tabs. The agent tab (idx 0)
// is unclosable; CloseTab errors on idx 0 or any out-of-range index. The tab is
// removed from Tabs regardless of whether the tmux teardown succeeds (best-
// effort, matching LocalBackend.Kill) so a broken session can't wedge the tab
// list. Unlike Kill this does not wait for the pane to exit: the worktree is not
// being removed, so there is no #802 delete race to guard against.
func (i *Instance) CloseTab(idx int) error {
	i.mu.Lock()
	if idx <= 0 || idx >= len(i.Tabs) {
		i.mu.Unlock()
		return fmt.Errorf("tab cannot be closed")
	}
	tab := i.Tabs[idx]
	i.Tabs = append(i.Tabs[:idx], i.Tabs[idx+1:]...)
	i.mu.Unlock()

	if tab.tmux == nil {
		return nil
	}
	if err := tab.tmux.Close(); err != nil {
		return fmt.Errorf("failed to close tab %q: %w", tab.Name, err)
	}
	return nil
}

// AttachShellTab reconnects this local instance's in-memory tab list to a shell
// tab that already exists server-side — one the daemon's CreateTab RPC just
// spawned out-of-band (#960 PR 2). It is the no-spawn counterpart of
// AddShellTab: the daemon owns the spawn (so its authoritative view holds the
// tab and can't be clobbered), and the TUI only needs to reflect the new tab
// locally for instant display. It binds to the EXACT tmux session the daemon
// derived (agent session name + "__" + name) and Restores (reconnects) it,
// mirroring restoreLocalTabs + LocalBackend.setupTabs, so the tab is immediately
// previewable/attachable without a second spawn that would collide on the name.
//
// name is the resolved tab name returned by the daemon. Local instances only —
// callers reject IsRemote() first. If a tab with that name is already present
// (e.g. a refresh raced ahead) this is a no-op returning the existing tab.
// Errors when the instance is not started or has no agent session/worktree.
func (i *Instance) AttachShellTab(name string) (*Tab, error) {
	i.mu.RLock()
	started := i.started
	agentTmux := i.tmuxLocked()
	gw := i.gitWorktree
	var existing *Tab
	for _, t := range i.Tabs {
		if t.Name == name {
			existing = t
			break
		}
	}
	i.mu.RUnlock()

	if existing != nil {
		return existing, nil
	}
	if !started || agentTmux == nil || gw == nil {
		return nil, fmt.Errorf("cannot attach a tab to an instance that is not started")
	}
	worktreePath := gw.GetWorktreePath()
	if worktreePath == "" {
		return nil, fmt.Errorf("cannot attach a tab without a worktree")
	}

	// Bind to the exact session name the daemon derived and ATTACH-ONLY to it —
	// never spawn. Pass empty workDir so a session that is missing surfaces as an
	// error instead of re-spawning (#1152). The daemon is the single writer that
	// owns every tmux spawn (#960); this is a pure TUI-side projection of a tab
	// the daemon already created. If the daemon killed the instance in the window
	// since our RLock, the session is gone, and re-spawning it here would create a
	// tmux session that escapes the daemon's Kill teardown and orphans over the
	// about-to-be-deleted worktree — the same #990 leak AddShellTab guards. Fail
	// cleanly and let the daemon's next Snapshot reconcile the tab away.
	tmuxName := agentTmux.SanitizedName() + "__" + name
	shellTmux := agentTmux.NewSiblingSession(tmuxName, defaultShell())
	if err := shellTmux.Restore(""); err != nil {
		return nil, fmt.Errorf("failed to reconnect shell tab: %w", err)
	}

	tab := newShellTab(shellTmux)
	tab.Name = name
	i.mu.Lock()
	// Re-check started under the write lock before appending, mirroring
	// AddShellTab: Kill is not serialized against attach and can have flipped
	// started=false (snapshotting Tabs for teardown) in the window since our
	// RLock. Nothing was spawned above (attach-only), so a lost race only needs to
	// release the local attach client we opened and drop the projection; the next
	// reconcile re-adds the tab if it still exists server-side.
	killed := !i.started
	title := i.Title
	if !killed {
		i.Tabs = append(i.Tabs, tab)
	}
	i.mu.Unlock()
	if killed {
		if cerr := shellTmux.CloseAttachOnly(); cerr != nil {
			log.WarningLog.Printf("attach shell tab to %q: releasing attach client after kill race: %v", title, cerr)
		}
		return nil, fmt.Errorf("session was killed during tab attach")
	}
	return tab, nil
}

// DropClosedTab removes the tab at idx from the in-memory list WITHOUT killing
// its tmux session (#960 PR 2). It is the no-kill counterpart of CloseTab, used
// when the daemon's CloseTab RPC has already torn the tmux session down: the
// daemon owns the kill+persist, and the TUI only needs to drop the now-dead tab
// from its local view for instant display. Killing again here would shell out a
// second tmux kill-session that errors ("session not found") on the already-gone
// session and surface a spurious failure. The agent tab (idx 0) is undroppable;
// errors on idx 0 or any out-of-range index, mirroring CloseTab.
func (i *Instance) DropClosedTab(idx int) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if idx <= 0 || idx >= len(i.Tabs) {
		return fmt.Errorf("tab cannot be closed")
	}
	i.Tabs = append(i.Tabs[:idx], i.Tabs[idx+1:]...)
	return nil
}

// ReconcileTabsFromData updates this started local instance's tab list to match
// `target`, the daemon's authoritative serialized tab list (#960 PR 3). The
// daemon is the single owner of tab state, so the TUI mirrors it: tabs the
// daemon added out-of-band (present in target, absent locally) are reconnected
// to their EXACT persisted tmux session by name — like restoreLocalTabs — and
// appended, so an out-of-band tab appears in the running TUI and is immediately
// previewable/attachable (the #959 "live display" fix); tabs the daemon closed
// (absent from target) are dropped locally WITHOUT re-killing their tmux session
// (the daemon already tore it down — killing again would error on the gone
// session). The agent tab (index 0) is never added or dropped: it is the
// instance's own session and is always present. Returns whether the local list
// changed. A no-op for a not-started instance, one without an agent session, or
// a remote instance (callers skip IsRemote() — remote tabs come from hook
// config, not the snapshot). Per-tab reconnect failures are collected into the
// returned error after every other change is applied, so one bad tab can't wedge
// the reconcile.
func (i *Instance) ReconcileTabsFromData(target []TabData) (bool, error) {
	i.mu.RLock()
	started := i.started
	agentTmux := i.tmuxLocked()
	gw := i.gitWorktree
	program := i.Program
	localNames := make(map[string]bool, len(i.Tabs))
	for _, t := range i.Tabs {
		localNames[t.Name] = true
	}
	i.mu.RUnlock()

	if !started || agentTmux == nil || gw == nil {
		return false, nil
	}
	worktreePath := gw.GetWorktreePath()

	targetNames := make(map[string]bool, len(target))
	for _, td := range target {
		targetNames[td.Name] = true
	}

	changed := false

	// Drop local non-agent tabs the daemon no longer lists. No kill: the daemon
	// owns the teardown and already closed the tmux session (#960 PR 3).
	for name := range localNames {
		if targetNames[name] {
			continue
		}
		if i.dropTabByName(name) {
			changed = true
		}
	}

	// Add daemon-listed tabs missing locally, reconnecting each to its exact
	// persisted tmux session by name so it is immediately attachable.
	var firstErr error
	for _, td := range target {
		if td.Kind == TabKindAgent || localNames[td.Name] {
			continue
		}
		if td.TmuxName == "" || worktreePath == "" {
			continue
		}
		kind := tabKindForData(td.Kind)
		// The sibling inherits the agent session's PTY factory / executor (real
		// in production, mock in tests), binding to the EXACT persisted name.
		// ATTACH-ONLY: pass empty workDir so a missing session errors instead of
		// re-spawning (#1152). Like AttachShellTab, this is a pure TUI-side
		// projection of daemon-owned tabs; the daemon is the single writer that
		// owns every spawn (#960). If the daemon killed the session in the race
		// window, re-spawning here would orphan a tmux session over the deleted
		// worktree. Skip the tab on failure and let the next snapshot reconcile it.
		ts := agentTmux.NewSiblingSession(td.TmuxName, tabProgram(kind, td.Command, program))
		if err := ts.Restore(""); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to reconnect tab %q: %w", td.Name, err)
			}
			continue
		}
		tab := &Tab{Name: td.Name, Kind: kind, Command: td.Command, tmux: ts}
		i.mu.Lock()
		// Re-check under the write lock: a concurrent reconcile/AddTab may have
		// added this name while we reconnected outside the lock.
		exists := false
		for _, t := range i.Tabs {
			if t.Name == td.Name {
				exists = true
				break
			}
		}
		if !exists {
			i.Tabs = append(i.Tabs, tab)
			changed = true
		}
		i.mu.Unlock()
	}
	return changed, firstErr
}

// dropTabByName removes the named non-agent tab from the in-memory list WITHOUT
// killing its tmux session — the no-kill counterpart of CloseTab used by
// ReconcileTabsFromData when the daemon has already torn the session down (#960
// PR 3). Returns whether a tab was removed. The agent tab (index 0) is never
// dropped.
func (i *Instance) dropTabByName(name string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for idx := 1; idx < len(i.Tabs); idx++ {
		if i.Tabs[idx].Name == name {
			i.Tabs = append(i.Tabs[:idx], i.Tabs[idx+1:]...)
			return true
		}
	}
	return false
}

// setTmuxLocked stores ts as the agent tab's tmux session, materializing the
// single Agent tab on first assignment so the agent session is always Tabs[0].
// Passing nil clears the session but leaves the tab in place (and is a no-op
// before the agent tab exists). Callers must hold i.mu for writing.
func (i *Instance) setTmuxLocked(ts *tmux.TmuxSession) {
	if len(i.Tabs) == 0 {
		if ts == nil {
			return
		}
		i.Tabs = []*Tab{newAgentTab(ts)}
		return
	}
	i.Tabs[0].tmux = ts
}

// ToInstanceData converts an Instance to its serializable form
func (i *Instance) ToInstanceData() InstanceData {
	i.mu.RLock()
	defer i.mu.RUnlock()

	data := InstanceData{
		ID:     i.ID,
		Title:  i.Title,
		Path:   i.Path,
		Branch: i.Branch,
		// Persist BOTH axes: Liveness is the new canonical value, Status the
		// legacy int kept for one release so a binary rolled back to before
		// #1195 still reads a sensible status (and as the read-fallback source
		// for records this build wrote before the split). SaveInstances skips
		// transient (Loading/Deleting) rows, so a persisted Status is always a
		// settled liveness value.
		Status:     i.statusLocked(),
		Liveness:   i.liveness,
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
	if i.remoteMeta != nil {
		data.RemoteMeta = i.remoteMeta
	}

	// Persist the usage-limit reset time only while the session is actually
	// limit-blocked (#1146). Gating on the liveness keeps a recovered session
	// from carrying a stale reset time to disk or into the snapshot — the
	// in-memory field lingers after ClearLimitReached but is never serialized.
	if i.liveness == LiveLimitReached {
		data.LimitResetAt = i.limitResetAt
	}

	// Persist each tab so the full agent+shell tab list survives a restart
	// (Sachin's hard requirement for #930): on reload FromInstanceData restores
	// each local tab's tmux session by its exact persisted name, reconnecting
	// live sessions across an af/daemon restart. Remote tabs (agent + optional
	// terminal) carry no tmux session, so they serialize with an empty TmuxName;
	// on restore HookBackend.Start re-derives them from the live terminal_cmd
	// config (syncRemoteTabs) rather than from this serialized list, so a
	// terminal_cmd added or removed while af was down is honored.
	for _, tab := range i.Tabs {
		td := TabData{Name: tab.Name, Kind: tab.Kind, Command: tab.Command}
		if tab.tmux != nil {
			td.TmuxName = tab.tmux.SanitizedName()
		}
		td.Conversation = conversationDataPtr(tab.Conversation)
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
	// Resolve the in-flight-op axis from the legacy status. A persisted record
	// is always settled (SaveInstances skips transients), but a SNAPSHOT of a
	// transient instance carries Loading/Deleting, which buildInstanceFromSnapshot
	// must reconstruct so the rebuilt row composes to the identical Status.
	inFlightOp := opForStatus(data.Status)
	instance := &Instance{
		ID:           data.ID,
		Title:        data.Title,
		Path:         data.Path,
		Branch:       data.Branch,
		liveness:     liveness,
		inFlightOp:   inFlightOp,
		limitResetAt: data.LimitResetAt,
		Height:       data.Height,
		Width:        data.Width,
		CreatedAt:    data.CreatedAt,
		UpdatedAt:    data.UpdatedAt,
		Program:      data.Program,
		AutoYes:      data.AutoYes,
		Prompt:       data.Prompt,
		userKilled:   data.UserKilled,
		remoteMeta:   data.RemoteMeta,
	}

	// Pick backend based on persisted BackendType.
	if data.BackendType == "remote" {
		hook, err := loadHookBackendForPath(data.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to load remote hooks config: %w", err)
		}
		instance.backend = hook
	} else {
		instance.backend = &LocalBackend{}

		// Preserve backward compatibility: when the branch_created_by_us
		// field is missing from persisted data (written before this field
		// was added), default to true. Old saved sessions were created
		// under the assumption that the session owned the branch, so
		// keeping that behavior avoids surprising changes on restore.
		branchCreatedByUs := true
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
			instance.Tabs = append(instance.Tabs, &Tab{
				Name:         td.Name,
				Kind:         kind,
				Command:      td.Command,
				Conversation: conversation,
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

// tabProgram resolves the program a tab's tmux session runs, by kind: the agent
// program for Agent tabs, $SHELL for Shell tabs, and the explicit command for
// Process tabs (falling back to $SHELL when empty).
func tabProgram(kind TabKind, command, agentProgram string) string {
	switch kind {
	case TabKindAgent:
		return agentProgram
	case TabKindProcess:
		if command != "" {
			return command
		}
		return defaultShell()
	default:
		return defaultShell()
	}
}

// defaultShell returns the user's $SHELL, falling back to /bin/sh — exactly the
// resolution the old UI terminal cache used (ui/terminal.go) before the shell
// tab was promoted onto the Instance.
func defaultShell() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return shell
}

// Options for creating a new instance
type InstanceOptions struct {
	// Title is the title of the instance.
	Title string
	// Path is the path to the workspace.
	Path string
	// Program is the program to run in the instance (e.g. "claude", "aider --model ollama_chat/gemma3:1b")
	Program string
	// If AutoYes is true, then
	AutoYes bool
	// ForceRemote forces the instance to use the remote hook backend,
	// even if the repo config would default to local.
	ForceRemote bool
	// InPlace attaches the session to the repo's existing working tree at its
	// current branch (`af sessions create --here`) instead of creating a new
	// git worktree+branch. The worktree is marked external so kill/cleanup
	// never removes the user's tree or branch. Local backend only.
	InPlace bool
}

// backendFactory constructs the Backend used by a new Instance. It is a
// package-level variable (not a hard-coded branch) so tests can inject a
// FakeBackend through SetBackendFactoryForTest without touching production
// code paths. Defaults to the real local/remote branching.
var backendFactory = defaultBackendFactory

func defaultBackendFactory(opts InstanceOptions, absPath string) (Backend, error) {
	if opts.ForceRemote {
		hook, err := loadHookBackendForPath(absPath)
		if err != nil {
			return nil, fmt.Errorf("remote hooks not configured for this repo: %w", err)
		}
		return hook, nil
	}
	return &LocalBackend{}, nil
}

// SetBackendFactoryForTest replaces the backend factory with f and returns a
// restore function. Intended for use in tests that need to swap in a
// FakeBackend so NewInstance-driven creation flows stay on the hot path.
func SetBackendFactoryForTest(f func(opts InstanceOptions, absPath string) (Backend, error)) func() {
	prev := backendFactory
	backendFactory = f
	return func() { backendFactory = prev }
}

// newSessionID mints a random RFC-4122 v4 UUID for an instance's stable identity
// (#1195). It is a package var so tests can inject deterministic IDs. crypto/rand
// is the entropy source; on the (near-impossible) read failure it falls back to a
// timestamp-derived value so session creation never blocks on entropy — still
// unique per call in practice, and the reconcile's title+CreatedAt fallback covers
// any theoretical collision.
var newSessionID = func() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func NewInstance(opts InstanceOptions) (*Instance, error) {
	t := time.Now()

	// An in-place session runs in the repo's local working tree; a remote
	// session has no local worktree at all — the two are contradictory.
	if opts.InPlace && opts.ForceRemote {
		return nil, fmt.Errorf("remote sessions cannot run in-place in the local repo working tree")
	}

	// Convert path to absolute
	absPath, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	backend, err := backendFactory(opts, absPath)
	if err != nil {
		return nil, err
	}

	return &Instance{
		ID:        newSessionID(),
		Title:     opts.Title,
		liveness:  LiveReady,
		Path:      absPath,
		Program:   opts.Program,
		Height:    0,
		Width:     0,
		CreatedAt: t,
		UpdatedAt: t,
		AutoYes:   opts.AutoYes,
		inPlace:   opts.InPlace,
		backend:   backend,
	}, nil
}

func (i *Instance) RepoName() (string, error) {
	if i.IsRemote() {
		return "", fmt.Errorf("remote instances do not have a local repo")
	}
	i.mu.RLock()
	started := i.started
	gw := i.gitWorktree
	i.mu.RUnlock()
	if !started {
		return "", fmt.Errorf("cannot get repo name for instance that has not been started")
	}
	if gw == nil {
		return "", fmt.Errorf("cannot get repo name for instance without a git worktree")
	}
	return gw.GetRepoName(), nil
}

// SetAutoYes sets the AutoYes flag under the instance mutex. Writers must use
// this rather than assigning i.AutoYes directly: TapEnter runs from the
// metadata-tick background goroutine and reads AutoYes under i.mu.RLock, so
// any unsynchronized write produces a data race (issue #563, regression from
// PR #560 which moved the tick off the bubbletea event loop).
func (i *Instance) SetAutoYes(autoYes bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.AutoYes = autoYes
}

// GetBranch returns the current worktree branch name under the Instance's
// mutex. Readers that run from goroutines other than the one mutating the
// instance (notably the bubbletea renderer) must use this accessor rather
// than reading i.Branch directly, or the race detector flags a write in
// LocalBackend.Start vs a read in InstanceRenderer.Render.
func (i *Instance) GetBranch() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Branch
}

// MarkUserKilled records kill intent on the instance (#1108). Callers persist
// the instance afterwards so the tombstone survives a daemon crash mid-kill.
func (i *Instance) MarkUserKilled() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.userKilled = true
}

// UserKilled reports whether an explicit kill was recorded for this instance.
func (i *Instance) UserKilled() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.userKilled
}

// firstTimeSetup is true if this is a new instance. Otherwise, it's one loaded from storage.
func (i *Instance) Start(firstTimeSetup bool) error {
	return i.backend.Start(i, firstTimeSetup)
}

// Kill terminates the instance and cleans up all resources
func (i *Instance) Kill() error {
	return i.backend.Kill(i)
}

// Recover re-establishes a Lost instance's backing session (#1108). Only the
// daemon's restore loop calls this; loads stay side-effect free (#970).
func (i *Instance) Recover() error {
	return i.backend.Recover(i)
}

// Respawn re-establishes the instance's backing session in place without a
// liveness precondition — the guard-free core of Recover. The usage-limit
// manual-retry (#1146, resumeFromLimit) uses it to re-spawn an agent that exited
// while blocked at a limit wall: that session is LiveLimitReached, which Recover's
// !Lost guard rejects, but the re-spawn mechanics are identical. The caller owns
// the precondition.
func (i *Instance) Respawn() error {
	return i.backend.Respawn(i)
}

// ArchiveTeardown tears down every tab's tmux session for an archive AND
// relocates the worktree to dest in one operation (#1028) — the tmux half of
// Kill, but it PRESERVES the record and MOVES the worktree instead of deleting
// it. It routes through the shared teardownTabs core in the archive mode, so the
// #802 "wait for every pane to exit before touching the worktree" ordering is
// shared code with Kill rather than the duplicated prose it was when the move
// lived in a separate daemon step (#1195 Phase 2b). It is deliberately
// best-effort for tmux (a stuck session only logs, mirroring Kill) and:
//   - keeps the AGENT tab's tmux binding (its session name) so a failed archive
//     can re-spawn it in place via the Lost-restore loop;
//   - drops the shell/process tabs entirely — only the agent session is brought
//     back on un-archive (Sachin's #1028 requirement);
//   - leaves gitWorktree and started untouched, so the daemon caller controls
//     the final state (started=false + Archived on success; Lost on a failed
//     move — returned here — where started stays true so the loop re-spawns the
//     agent).
//
// Returns the worktree-move error (nil on success). Local instances only —
// remote sessions have no local tmux/worktree and the daemon rejects archiving
// them before reaching here.
func (i *Instance) ArchiveTeardown(dest string) error {
	return i.teardownTabs(teardownArchive{dest: dest})
}

// SetArchived flips the instance into the inert Archived state atomically:
// started=false (no tmux binding backs it) and liveness=Archived, clearing any
// in-flight op. Called by the daemon after a successful archive move.
func (i *Instance) SetArchived() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.started = false
	i.liveness = LiveArchived
	i.inFlightOp = OpNone
}

// RestoreArchivedWorktree moves this instance's archived worktree back to dest
// and re-registers it against the origin repo (#1028). Surfaces git.ErrRepoGone
// when the repo has been deleted so the caller can leave the archive intact.
func (i *Instance) RestoreArchivedWorktree(dest string) error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot restore %q: instance has no worktree", i.Title)
	}
	return gw.RestoreWorktreeTo(dest)
}

// RestoreFromArchive re-spawns an archived instance's agent after its worktree
// has been moved back into place (#1028), flipping it live. It marks the
// instance started + Lost so the Recover re-spawn path is eligible (the same
// re-spawn the #1108 Lost-restore loop drives), then Recover brings the agent
// session up and sets Running (markLive clears the OpRestoring fence). On a
// Recover failure the instance is dropped to a plain Lost (op cleared), so the
// daemon's Lost-restore loop keeps retrying — the worktree is already back in
// place, so the session self-heals rather than stranding as Archived with no
// tmux. Only the agent tab is restored (shell/process tabs were dropped at
// archive time, per #1028).
//
// liveness is set to Lost (so Recover's ==Lost gate accepts it) and OpRestoring
// fences the re-spawn window: the daemon poll skips an instance with an
// in-flight op, so it never probes the half-spawned session and marks it Lost
// out from under the restore. This replaces the old "park it in Lost purely to
// trigger the re-spawn loop" overload (#1195).
func (i *Instance) RestoreFromArchive() error {
	// Enter the restore fence through the chokepoint (#1195 Phase 2d): BeginRestore
	// is legal only from Archived and sets started=true + Lost + OpRestoring — the
	// exact head this used to write by hand, now enforcing I3 (a restore may begin
	// only from an archived session; no double-restore).
	if err := i.Transition(BeginRestore()); err != nil {
		return err
	}
	if err := i.backend.Recover(i); err != nil {
		// Re-spawn failed: drop the fence to a plain Lost (started left true) so
		// the #1108 restore loop owns the retry against the now-restored worktree.
		_ = i.Transition(AbortRestoreToLost())
		return err
	}
	return nil
}

// CloseAttachOnly releases the resources this instance opened to view or drive
// its session (a tmux attach PTY, a remote preview process) without destroying
// the session, worktree, or remote record. Use it — never Kill — to discard a
// duplicate Instance built from disk that lost a race to the canonical tracked
// Instance (#867); see Backend.CloseAttachOnly.
func (i *Instance) CloseAttachOnly() error {
	return i.backend.CloseAttachOnly(i)
}

func (i *Instance) Preview() (string, error) {
	return i.backend.Preview(i)
}

func (i *Instance) HasUpdated() (updated bool, hasPrompt bool, content string) {
	return i.backend.HasUpdated(i)
}

// CheckAndHandleTrustPrompt checks for and dismisses the trust prompt for supported programs.
func (i *Instance) CheckAndHandleTrustPrompt() bool {
	return i.backend.CheckAndHandleTrustPrompt(i)
}

// TapEnter sends an enter key press to the tmux session if AutoYes is enabled.
func (i *Instance) TapEnter() {
	i.backend.TapEnter(i)
}

func (i *Instance) Attach() (chan struct{}, error) {
	return i.backend.Attach(i)
}

func (i *Instance) SetPreviewSize(width, height int) error {
	return i.backend.SetPreviewSize(i, width, height)
}

// GetGitWorktree returns the git worktree for the instance
func (i *Instance) GetGitWorktree() (*git.GitWorktree, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.started {
		return nil, fmt.Errorf("cannot get git worktree for instance that has not been started")
	}
	return i.gitWorktree, nil
}

// GetWorktreePath returns the worktree path for the instance, or empty string if unavailable
func (i *Instance) GetWorktreePath() string {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()

	if gw == nil {
		return ""
	}
	return gw.GetWorktreePath()
}

// GetRepoPath returns the resolved git repo path stored in the instance's
// worktree, or empty string when no worktree is attached (e.g. a remote-
// backend instance). Callers using the result to derive a repo ID must
// fall back to Instance.Path when this is empty (#667).
func (i *Instance) GetRepoPath() string {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()

	if gw == nil {
		return ""
	}
	return gw.GetRepoPath()
}

func (i *Instance) Started() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.started
}

// IsExternalWorktree reports whether the instance's worktree is external/in-place
// (`af sessions create --here`, or a legacy external record) — the same flag
// MoveWorktree checks. Such a worktree is the user's own working tree and must
// never be relocated, so the daemon rejects archiving it (#1028). Returns false
// when the instance has no worktree yet.
func (i *Instance) IsExternalWorktree() bool {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	return gw != nil && gw.IsExternalWorktree()
}

// SetTitle sets the title of the instance. Returns an error if the instance has started.
// We cant change the title once it's been used for a tmux session etc.
func (i *Instance) SetTitle(title string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.started {
		return fmt.Errorf("cannot change title of a started instance")
	}
	i.Title = title
	return nil
}

// TmuxAlive returns true if the underlying session is alive.
// For remote backends this delegates to IsAlive.
func (i *Instance) TmuxAlive() bool {
	return i.backend.IsAlive(i)
}

// ResolvedAgent returns the canonical agent (one of tmux.SupportedPrograms)
// this instance's pane will actually run, or "" when the resolved command
// runs no known agent — e.g. a program_overrides entry pointing an agent name
// at a plain shell (#1131). Agent-specific behavior (readiness heuristics,
// trust-prompt handling, flag injection) must key off this, never off
// Instance.Program: Program is the config-name enum the instance was created
// with, and an override may point it at a different program entirely (#1116).
//
// Once the tmux session exists, its program string (override-resolved and
// flag-injected by Start) is the ground truth. Before Start — or in tests
// that never attach a tmux session — detection falls back to the raw Program
// value, which also covers legacy free-form persisted values like
// "/home/foo/bin/claude --plugin-dir x" (#677).
func (i *Instance) ResolvedAgent() string {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts != nil {
		if p := ts.Program(); strings.TrimSpace(p) != "" {
			return tmux.DetectAgentFromCommand(p)
		}
	}
	return tmux.DetectAgentFromCommand(i.Program)
}

// GetPRInfo returns the associated GitHub PR info, or nil if none.
func (i *Instance) GetPRInfo() *git.PRInfo {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.prInfo
}

// SetPRInfo sets the associated GitHub PR info.
func (i *Instance) SetPRInfo(info *git.PRInfo) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prInfo = info
	i.prInfoLastFetched = time.Now()
}

// PRInfoAge returns how long ago PR info was last fetched. Returns a very
// large duration if PR info has never been fetched in this process.
func (i *Instance) PRInfoAge() time.Duration {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.prInfoLastFetched.IsZero() {
		return time.Duration(1<<62 - 1)
	}
	return time.Since(i.prInfoLastFetched)
}

// MarkPRInfoFetched bumps the fetch timestamp without touching the cached
// value. Used after a transient fetch error so we don't re-try on every
// subsequent selection change.
func (i *Instance) MarkPRInfoFetched() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prInfoLastFetched = time.Now()
}

// FetchPRInfoSnapshot returns the data needed to fetch PR info for this
// instance off the main event loop. The returned repoPath is empty when the
// instance is not ready for fetching (not started, no worktree, or remote).
func (i *Instance) FetchPRInfoSnapshot() (repoPath, branch string) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.started || i.gitWorktree == nil {
		return "", ""
	}
	return i.gitWorktree.GetRepoPath(), i.Branch
}

// SendPrompt sends a prompt to the session
func (i *Instance) SendPrompt(prompt string) error {
	return i.backend.SendPrompt(i, prompt)
}

// SendPromptCommand sends a prompt using a more reliable command-based approach.
// This is more reliable for headless/scheduled runs where the PTY may not persist.
func (i *Instance) SendPromptCommand(prompt string) error {
	return i.backend.SendPromptCommand(i, prompt)
}

// PreviewFullHistory captures the entire session output including full scrollback history
func (i *Instance) PreviewFullHistory() (string, error) {
	return i.backend.PreviewFullHistory(i)
}

// SetTmuxSession sets the agent tab's tmux session for testing purposes,
// materializing the single Agent tab if needed.
func (i *Instance) SetTmuxSession(session *tmux.TmuxSession) {
	i.setTmuxLocked(session)
}

// SetStartedForTest toggles the started flag for testing purposes. Prefer
// Start() in non-test code; this exists so unit tests can exercise flows
// gated on Started() without spinning up a real tmux session.
func (i *Instance) SetStartedForTest(started bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.started = started
}

// SetGitWorktreeForTest assigns a git worktree to this instance. Test-only:
// the real flow sets this inside LocalBackend.Start, which isn't available
// in unit tests that use FakeBackend.
func (i *Instance) SetGitWorktreeForTest(gw *git.GitWorktree) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.gitWorktree = gw
}

// AddTabForTest appends a tmux-less tab record. Test-only: UI tests (the
// sidebar tree, tab labels) need instances with a populated tab LIST without
// spinning up real tmux sessions; the tab is never attachable or previewable.
func (i *Instance) AddTabForTest(name string, kind TabKind) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Tabs = append(i.Tabs, &Tab{Name: name, Kind: kind})
}

// SendKeys sends keys to the underlying session. For remote backends this
// returns an explicit error since raw key injection is not supported.
func (i *Instance) SendKeys(keys string) error {
	return i.backend.SendKeys(i, keys)
}

// IsRemote returns true if this instance uses the remote hook backend.
func (i *Instance) IsRemote() bool {
	if i.backend == nil {
		return false
	}
	return i.backend.Type() == "remote"
}

// SupportsRemoteTerminal reports whether this instance can open an interactive
// terminal on its remote machine — i.e. it uses the remote hook backend and
// the optional terminal_cmd hook is configured (#843).
func (i *Instance) SupportsRemoteTerminal() bool {
	hb, ok := i.backend.(*HookBackend)
	return ok && hb.HasTerminalCmd()
}

// AttachRemoteTerminal opens an interactive terminal on the remote machine via
// the terminal_cmd hook. The returned channel is closed when the user detaches
// or the terminal_cmd process exits. Errors when the instance is not backed by
// remote hooks or terminal_cmd is not configured.
func (i *Instance) AttachRemoteTerminal() (chan struct{}, error) {
	hb, ok := i.backend.(*HookBackend)
	if !ok {
		return nil, fmt.Errorf("remote terminal is only available for remote sessions")
	}
	return hb.AttachTerminal(i)
}

// GetBackend returns the backend for the instance (mainly for testing).
func (i *Instance) GetBackend() Backend {
	return i.backend
}

// SetBackend sets the backend for the instance (mainly for testing).
func (i *Instance) SetBackend(b Backend) {
	i.backend = b
}
