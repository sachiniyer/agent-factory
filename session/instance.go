package session

import (
	"fmt"
	"os"
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
	// (Running/Ready/Lost/Archived/…), inFlightOp is the transient client
	// operation overlaid on it (Creating/Killing/Archiving/Restoring). Snapshots
	// carry the op so secondary TUIs converge; disk persistence strips it. The
	// legacy Status enum is derived from them via the GetStatus/SetStatus shim in
	// liveness.go. Both are mutex-protected.
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

	// agentSrv is the cached per-instance AgentServer (#1592 Phase 2 PR5). Cached
	// rather than reconstructed per call because its data plane holds stateful
	// pieces — the PTY output ring buffer and the fan-out subscriber set — that
	// must persist across calls; a fresh server each call would drop subscribers
	// and lose the replay buffer. Lazily built by AgentServer(), guarded by
	// agentSrvMu (a dedicated mutex, not i.mu, so building the server never
	// contends with the session-state fields i.mu guards). Interface-typed since
	// #1592 Phase 4 PR2: AgentServer() returns the local in-process impl by
	// default, or a remoteAgentServer when remoteClient is set.
	agentSrv   AgentServer
	agentSrvMu sync.Mutex
	// remoteClient is the runtime handle selecting the REMOTE agent-server impl
	// (#1592 Phase 4 PR2): when non-nil, AgentServer() returns a remoteAgentServer
	// driving the `af agent-server` this points at, instead of the local in-process
	// runtime. Built once at NewInstance from InstanceOptions.RemoteAgentServer (so
	// a bad endpoint fails there, keeping AgentServer() infallible) and nil for
	// every local session — the default path is provably unchanged. DARK in PR2: no
	// runtime sets it yet (PR3-PR5).
	remoteClient *remoteAgentClient
}

// tabTmuxSession returns the tmux session backing the tab at idx (0 is the agent
// tab), or nil when the instance is not started, is remote, or idx is out of
// range. It is the tab-aware binding point the clientless data plane subscribes
// per pane (#1592 Phase 2 PR6). Takes i.mu, so callers must not already hold it.
func (i *Instance) tabTmuxSession(idx int) *tmux.TmuxSession {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.started {
		return nil
	}
	return i.tabTmuxAtLocked(idx)
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
// callers reject backends without TabManagement first. If a tab with that name
// is already present
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
	if idx <= 0 || idx >= len(i.Tabs) {
		i.mu.Unlock()
		return fmt.Errorf("tab cannot be closed")
	}
	tab := i.Tabs[idx]
	i.Tabs = append(i.Tabs[:idx], i.Tabs[idx+1:]...)
	i.mu.Unlock()

	// Release the TUI-side attach PTY (ptmx fd + blocked cmd.Wait goroutine) the
	// dropped tab held, matching CloseTab. No kill: the daemon's CloseTab RPC
	// already tore the tmux session down (#960 PR 2), so CloseAttachOnly only
	// releases this client's attach resources. Done outside the lock so the tmux
	// teardown never runs while holding i.mu.
	if tab.tmux != nil {
		if err := tab.tmux.CloseAttachOnly(); err != nil {
			log.WarningLog.Printf("DropClosedTab: releasing attach client for tab %q: %v", tab.Name, err)
		}
	}
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
// a remote instance (callers skip backends without TabManagement — remote tabs
// come from hook config, not the snapshot). Per-tab reconnect failures are collected into the
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
	var dropped *Tab
	for idx := 1; idx < len(i.Tabs); idx++ {
		if i.Tabs[idx].Name == name {
			dropped = i.Tabs[idx]
			i.Tabs = append(i.Tabs[:idx], i.Tabs[idx+1:]...)
			break
		}
	}
	i.mu.Unlock()
	if dropped == nil {
		return false
	}
	// Release the TUI-side attach PTY the dropped tab held — its ptmx fd and the
	// blocked cmd.Wait goroutine — mirroring CloseTab/AttachShellTab. No kill: the
	// daemon already tore the tmux session down (#960 PR 3), so this only releases
	// this client's attach resources. Done outside the lock so the tmux teardown
	// never runs while holding i.mu.
	if dropped.tmux != nil {
		if err := dropped.tmux.CloseAttachOnly(); err != nil {
			log.WarningLog.Printf("dropTabByName %q: releasing attach client: %v", name, err)
		}
	}
	return true
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
