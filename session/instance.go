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
	// + shell/process tabs; web tabs have no tmux and survive with their URLs,
	// #1809) and MOVED the worktree out to the global archive
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
	// TaskID is the id of the task whose delivery spawned this session, empty for
	// a user-created one (#1892). It is the daemon's association between a task
	// delivery and its session, replacing title-prefix guessing; the watch-task
	// concurrency limit counts a task's in-flight sessions by it. Immutable after
	// construction, so cross-goroutine readers may read it without the mutex
	// (like ID and Title).
	TaskID string
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
	// taskRunActive is THE fact the watch-task concurrency cap is about (#1892):
	// has this session's task run finished yet? It is true from creation for a
	// task-spawned session and flips false — once, permanently — when the AGENT
	// first goes idle, or when startup settles terminal-unknown without ever
	// establishing a runnable session. Either outcome means no run remains that a
	// later poll could observe finishing.
	//
	// It is a stored fact rather than something derived at read time because every
	// neighbouring signal answers a DIFFERENT question, and reconstructing the run
	// from them is what produced two separate cap breaches:
	//
	//   - liveness says whether the daemon can SEE the session. Lost is
	//     indistinguishable between a run that finished hours ago and one
	//     interrupted mid-flight, so deciding at the Lost edge let a completed run
	//     reacquire a slot from the grave.
	//   - inFlightOp says the DAEMON is doing something — and archiving or killing a
	//     session is teardown, not the agent working. Reading "any op ⇒ busy" made a
	//     finished session that failed to archive (LiveReady + OpArchiving →
	//     AbortArchiveToLost) look like an interrupted run and claim a slot.
	//
	// So the run's own lifetime is recorded on the run's own edges: it begins when
	// the session is created for a delivery and ends when the agent goes idle or
	// startup reaches its explicit terminal-unknown boundary. Neither has to be
	// inferred later from a neighbouring state. Persisted, because an outage that
	// loses sessions is the same event that restarts the daemon.
	//
	// It never flips back to true: a capped task creates one session per event (a
	// cap and a target_session are mutually exclusive — see task.ValidateTrigger),
	// so a session has exactly one run. Work a user starts in that session
	// afterwards is theirs, not the task's, and must not consume the task's cap.
	taskRunActive bool
	// limitResetAt is the parsed usage-limit reset time (#1146), display-only in
	// PR2: set alongside liveness == LiveLimitReached when the pane shows a limit
	// banner carrying a parseable reset time (zero when it carried none). Read
	// only while limit-blocked (LimitResetAt/ToInstanceData gate on the liveness),
	// so a lingering value on a recovered session never surfaces. Persisted and
	// carried in the daemon snapshot so the badge survives a restart; PR3's
	// auto-resume scheduler reads it. Mutex-protected.
	limitResetAt time.Time
	// agentModelChange is a live, projection-only diagnostic supplied by the
	// running agent-server. It is mutex-protected and deliberately omitted from
	// durable restore state; see AgentModelChange and InstanceData.ForStorage.
	agentModelChange *AgentModelChange
	// stateEpoch is the generation counter for the lifecycle state above — the two
	// axes plus limitResetAt — bumped by every writer that actually changes one of
	// them (#2135). It is how an observer that decided from a captured pane learns
	// its decision has been superseded by a newer transition before it applies it;
	// see state_epoch.go. Mutex-protected, in-memory only: it describes a window
	// between an observation and its apply, and no such window survives a restart.
	stateEpoch uint64
	// agentRuntimeGeneration identifies the concrete agent process currently
	// owning the Agent tab. Async conversation capture binds to this generation,
	// so a result from a replaced process cannot write through a later handoff or
	// recovery merely because the Instance pointer (or even agent name) matches.
	// In-memory only: no capture goroutine survives a daemon restart.
	agentRuntimeGeneration uint64
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
	// Prompt is the initial prompt to pass to the instance on startup
	Prompt string
	// pendingHandoffMission is a rendered takeover brief whose delivery has not
	// been durably confirmed. It is separate from Prompt: Prompt is the user's
	// durable goal, while this value includes one handoff's generated context and
	// must be cleared once that exact delivery lands. Persisting it closes the
	// daemon-crash window between a runtime swap and readiness.
	pendingHandoffMission string
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

	// startupStateUnknown means a fresh create crossed the process-spawn boundary
	// but af could not determine whether its runtime came up. The record is kept
	// inert: no liveness probe, restore, or automatic teardown may turn that
	// uncertainty into permission to delete its workspace (#2207). Persisted so a
	// daemon restart cannot re-arm the destructive path.
	startupStateUnknown bool

	// prInfo stores the associated GitHub PR info
	prInfo *git.PRInfo
	// prInfoLastFetched is the wall-clock time of the most recent PR info
	// fetch. Not persisted — restored instances start with a zero value so
	// the first lazy fetch on selection always runs. Used to debounce
	// repeated fetches when the user cycles the sidebar.
	prInfoLastFetched time.Time

	// backend abstracts session lifecycle (local tmux+git vs off-box runtimes).
	backend Backend

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
	// and lose the replay buffer. Lazily built by AgentServer() and guarded by i.mu,
	// the SAME lock as remoteClient/runtimeTeardown (the fields that select which
	// impl it is): a cache and its source fields must share one mutex so a restore
	// that swaps the fields and clears the cache is atomic wrt the poll — a
	// dedicated agentSrvMu split them and let AgentServer() rebuild the cache from a
	// pre-restore snapshot, pinning a torn-down endpoint (#1729). Interface-typed
	// since #1592 Phase 4 PR2: AgentServer() returns the local in-process impl by
	// default, or a remoteAgentServer when remoteClient is set.
	agentSrv AgentServer
	// remoteClient is the runtime handle selecting the REMOTE agent-server impl
	// (#1592 Phase 4 PR2): when non-nil, AgentServer() returns a remoteAgentServer
	// driving the `af agent-server` this points at, instead of the local in-process
	// runtime. Built once at NewInstance from InstanceOptions.RemoteAgentServer (so
	// a bad endpoint fails there, keeping AgentServer() infallible) and nil for
	// every local session — the default path is provably unchanged. Set by the
	// docker runtime (#1592 Phase 4 PR4) from its provisioned container; nil for
	// local/hook sessions.
	remoteClient *remoteAgentClient
	// runtimeTeardown reaps the off-box sandbox a runtime provisioned (#1592
	// Phase 4 PR4): remove the container, SSH directory, or hook-provisioned
	// workspace. Set at NewInstance from the runtime's ProvisionResult and run by
	// the remote agent-server's Kill AFTER it tears the in-sandbox workspace down
	// over REST. nil only for local sessions. Each runtime serializes repeated
	// calls; docker/SSH deliberately retry outcomes whose completion is unknown.
	runtimeTeardown func() error
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
// mutex. The returned slice is a copy, so callers (the UI tab bar) can iterate
// it without racing concurrent tab mutation.
//
// The *Tab elements are the LIVE pointers, not copies, and callers read their
// Name/Kind/ID off-lock (tree.TabLabels on the render path, the TUI's
// tabNameAt/tabIndexByName). That is safe because those fields are never
// assigned in place once a tab is in i.Tabs: a tab's name can change, but the
// writers that change it — RenameTab and ReconcileTabsFromData — swap in a
// COPY carrying the new value (replaceTabFieldLocked) instead of writing the
// object a reader is already holding. So a snapshot keeps reading the values it
// was taken with, and the next GetTabs observes the new ones. Anything that
// wants to change one of those fields must go through replaceTabFieldLocked;
// assigning to a handed-out tab's Name is a data race, not a stale read (#1930).
//
// This does NOT extend to the whole struct: tmux and Conversation are still
// assigned IN PLACE under i.mu (setTmuxLocked, setupTabs' dead-shell
// replacement, teardown's ref clearing). The package does read those off a
// snapshot in places — setupTabs and teardownTabs both capture tabs under the
// lock and work outside it — and that is safe only because the daemon's
// per-instance op-lock serializes start/teardown against every other mutation.
// That discipline lives in the daemon, not in this type: prefer the locking
// accessors (TabTmuxByID, ToInstanceData), and if you read tmux/Conversation off
// a snapshot, know that is what you are leaning on.
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

// TabIDAt returns the stable id (#1738) of the tab at ordinal idx, and whether
// idx is in range. It is the index→id direction the data plane keys its per-tab
// broker on, so a broker follows its tab across a reorder/close instead of being
// pinned to a shifting ordinal.
func (i *Instance) TabIDAt(idx int) (string, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if idx < 0 || idx >= len(i.Tabs) {
		return "", false
	}
	return i.Tabs[idx].ID, true
}

// tabTmuxTargetAt resolves an ordinal to both the stable broker key and its tmux
// target under one lock. Reading those in separate calls can pair one tab's ID
// with a sibling's tmux when a close shifts the roster between the calls (#2200).
func (i *Instance) tabTmuxTargetAt(idx int) (id string, ts *tmux.TmuxSession, exists bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if idx < 0 || idx >= len(i.Tabs) {
		return "", nil, false
	}
	tab := i.Tabs[idx]
	if !i.started {
		return tab.ID, nil, true
	}
	return tab.ID, tab.tmux, true
}

// TabTmuxByID resolves a tab's stable id (#1738) DIRECTLY to the tmux session it
// currently backs, under a SINGLE lock acquisition. It is the atomic primitive the
// id-addressed data plane binds on: resolving an id to an ordinal and then that
// ordinal to a tmux session takes i.mu twice, and a concurrent close/reorder
// between the two makes the second lookup land on a DIFFERENT tab — exactly the
// misroute the stable id exists to prevent (#1779). Resolving both under one lock
// closes that window.
//
// The two return values answer two DIFFERENT questions, and callers must not
// conflate them:
//
//   - exists=false — the id names no tab at all: it was closed, or never minted.
//     This is the "gone" the id-addressed plane refuses on.
//   - exists=true, ts=nil — the tab is real but has no local PTY right now: the
//     instance has not started, or it is a remote runtime with no local tmux. NOT
//     gone; a caller must not report it as such, since a not-yet-started tab may
//     still come up and a client should keep addressing it.
func (i *Instance) TabTmuxByID(id string) (ts *tmux.TmuxSession, exists bool) {
	if id == "" {
		return nil, false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	for idx, t := range i.Tabs {
		if t.ID != id {
			continue
		}
		if !i.started {
			return nil, true // the tab exists; it just has no live PTY yet
		}
		return i.tabTmuxAtLocked(idx), true
	}
	return nil, false
}

// TabTargetByID resolves a tab's stable id (#1738) DIRECTLY to what the web-tab
// proxy addresses it by — its kind, and the target URL a TabKindWeb tab stores —
// under a SINGLE lock acquisition. It is TabTmuxByID's counterpart for the iframe
// plane, and exists for the same reason: id→ordinal followed by ordinal→tab takes
// i.mu TWICE, and a concurrent close between the two lands the second lookup on a
// DIFFERENT tab.
//
// A bounds check does not close that window, because the racing list is SHORTER,
// not out of range: with tabs [agent, A, B, C], resolving B yields ordinal 2, and
// a close of A before the second lookup leaves [agent, B, C] — where ordinal 2 is
// now C, in range and wrong. The proxy would then serve C's dev server under B's
// stable id, which is the exact misroute keying the route by id (#1810) exists to
// prevent.
//
// url is "" for every kind but TabKindWeb; a VSCODE tab deliberately stores none
// (its editor is resolved per request), so callers must not read absence of a URL
// as absence of a tab — that is what exists is for.
func (i *Instance) TabTargetByID(id string) (kind TabKind, url string, exists bool) {
	if id == "" {
		return 0, "", false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	for _, t := range i.Tabs {
		if t.ID == id {
			return t.Kind, t.URL, true
		}
	}
	return 0, "", false
}

// TabIndexByID returns the CURRENT ordinal of the tab with stable id (#1738),
// and whether such a tab exists. It is the id→index resolution the stream
// endpoint runs per operation: a client addresses a tab by its stable id and the
// daemon maps it to wherever that tab now sits, so a reorder/close on another
// client can never make the client's captured position refer to a different tab.
// An empty id never matches (a legacy/absent id is not addressable by id).
//
// Prefer a single-lock primitive (TabTmuxByID, TabTargetByID) where one exists
// for what the caller actually needs: an ordinal handed back to a SECOND lookup
// reopens the close/reorder window this resolution is meant to close.
func (i *Instance) TabIndexByID(id string) (int, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.tabIndexByIDLocked(id)
}

// TabAlive reports whether the tab at idx has a live tmux session, as a LOSSY
// bool: true means "alive OR could-not-determine (wedged/timeout)", false means
// "no binding, or definitively gone". It is deliberately kept lossy because its
// only consumers (ui/tab_pane.go) act solely on !TabAlive — they swap to the
// "Terminal session not available" fallback. A wedged server reads as alive here,
// which keeps the read-only TUI rendering the pane instead of falsely declaring a
// merely-unreachable terminal dead: the safe direction for a view (#1962). A
// caller that needs existence as EVIDENCE must use TmuxSession.ProbeSession
// directly and handle !known.
func (i *Instance) TabAlive(idx int) bool {
	i.mu.RLock()
	ts := i.tabTmuxAtLocked(idx)
	i.mu.RUnlock()
	return ts != nil && ts.ExistsOrUnknown()
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

// AttachShellTab reconnects this local instance's in-memory tab list to a shell
// tab that already exists server-side — one the daemon's CreateTab RPC just
// spawned out-of-band (#960 PR 2). It is the no-spawn counterpart of
// AddShellTab: the daemon owns the spawn (so its authoritative view holds the
// tab and can't be clobbered), and the TUI only needs to reflect the new tab
// locally for instant display. It binds to the EXACT tmux session the daemon
// spawned and Restores (reconnects) it, mirroring restoreLocalTabs +
// LocalBackend.setupTabs, so the tab is immediately previewable/attachable
// without a second, colliding spawn.
//
// name and tmuxName are BOTH the daemon's, as returned by CreateTab. tmuxName is
// passed, not re-derived as "<agent>__<name>", because the two are independent
// namespaces (#1957, see tab_names.go): after a rename the daemon spawns
// "…__shell-2" for a tab named "shell", and re-deriving would bind this
// projection to the OLDER tab's still-live session. Empty falls back to the
// derivation — right for its only cause, a daemon predating the field.
//
// Local instances only — callers reject backends without TabManagement first. A
// tab with that name already present (a refresh raced ahead) makes this a no-op
// returning it. Errors when the instance is not started or has no session.
func (i *Instance) AttachShellTab(name, tmuxName string) (*Tab, error) {
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

	// Bind to the exact session the daemon spawned and ATTACH-ONLY to it — never
	// spawn. Pass empty workDir so a session that is missing surfaces as an
	// error instead of re-spawning (#1152). The daemon is the single writer that
	// owns every tmux spawn (#960); this is a pure TUI-side projection of a tab
	// the daemon already created. If the daemon killed the instance in the window
	// since our RLock, the session is gone, and re-spawning it here would create a
	// tmux session that escapes the daemon's Kill teardown and orphans over the
	// about-to-be-deleted worktree — the same #990 leak AddShellTab guards. Fail
	// cleanly and let the daemon's next Snapshot reconcile the tab away.
	if tmuxName == "" {
		tmuxName = agentTmux.SanitizedName() + tmuxTabSeparator + name
	}
	shellTmux := agentTmux.NewSiblingSession(tmuxName, defaultShell())
	if err := shellTmux.Restore(""); err != nil {
		return nil, fmt.Errorf("failed to reconnect shell tab: %w", err)
	}

	tab := newShellTab(shellTmux)
	tab.Name = name
	// The daemon owns this tab's stable id (#1738) — it minted and persisted one in
	// its CreateTab. Don't invent a competing local id: leave it empty so the tab is
	// addressed positionally (safe: the TUI just created it, single-client) until the
	// next snapshot's ReconcileTabsFromData adopts the daemon's authoritative id.
	tab.ID = ""
	i.mu.Lock()
	// Re-check the teardown fence under the write lock before appending, mirroring
	// AddShellTab: Kill is not serialized against attach and can have flipped
	// started=false (snapshotting Tabs for teardown) in the window since our
	// RLock, and an archive teardown+move keeps started=true and raises OpArchiving
	// over that same window instead (#1195) — which the started-only recheck this
	// shipped with never saw, even though the caller gates on HasInFlightOp before
	// the daemon round-trip precisely because the op matters (#2100, the sibling of
	// the reconcile's missing recheck). Nothing was spawned above (attach-only), so
	// a lost race only needs to release the local attach client we opened and drop
	// the projection; the next reconcile re-adds the tab if it still exists
	// server-side.
	killed := !i.started || i.tabSpawnBlockedLocked() != nil
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
// previewable/attachable (the #959 "live display" fix). A tmux-less kind (web,
// vscode — see TabKind.HasTmux) has no session to reconnect and is appended
// directly, so it lands in the TUI as its placeholder pane rather than being
// mistaken for a tab whose tmux session went missing; tabs the daemon closed
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
	i.mu.RUnlock()

	if !started || agentTmux == nil || gw == nil {
		return false, nil
	}
	worktreePath := gw.GetWorktreePath()

	changed := false

	// The reconcile keys on the STABLE TAB ID (#1738), not the name
	// (#1886/#1905). Names are reused on close+recreate, so a name-keyed reconcile
	// reported "unchanged" for an out-of-band close+recreate and then silently
	// re-pointed the local tab's id at the NEW tab — leaving an open pane bound to
	// a tab that no longer exists, showing a different process. Keyed on the id,
	// that is a drop of the old id plus an add of the new one, which reports a
	// change and lets the pane layer close the orphaned pane.
	//
	// Name remains the join key ONLY where there is no id to key on: a local tab
	// materialized by AttachShellTab carries an empty id on purpose and adopts the
	// daemon's below, and a legacy roster row written before #1738 has none.
	// A local id that matches NO target id is treated as a different tab (drop +
	// add) rather than adopting the target's id: those two cases are
	// indistinguishable from here, and re-pointing the id is the silent-wrong-target
	// failure #1886 is about, whereas drop+add is a visible, self-healing blip.

	// Adopt the daemon's authoritative id for ID-LESS local tabs, by name-join. The
	// daemon is the single owner of tab identity (#960); AttachShellTab leaves the
	// id empty on purpose, so this is the bootstrap that makes the tab addressable
	// by id at all. Runs FIRST so the id-keyed passes below see it. Not a visible
	// change: the id is internal addressing, not display state.
	//
	// The AGENT tab additionally adopts over a NON-EMPTY local id, because it is the
	// only row the id-keyed passes below can never repair: it is never dropped or
	// re-added (it is the instance's own session, always at index 0), so the
	// close+recreate that heals a diverged id for every other kind cannot reach it,
	// and a stale id would stick FOREVER. That divergence is ordinary, not exotic:
	// restoreLocalTabs MINTS an id for a legacy pre-#1738 row, so a TUI and a daemon
	// loading the same id-less record independently mint DIFFERENT ids — one plain
	// daemon restart (every upgrade does one) over a not-yet-persisted backfill is
	// enough. From then on every preview/live/attach addresses the agent by an id the
	// daemon cannot resolve, and because the caller DID supply a tab_id there is no
	// ordinal fallback — it is ErrTabGone (see TabAddressableServer), i.e. a blank,
	// unattachable agent pane with no way out.
	//
	// Name is the right join key here precisely where id is not: both sides derive
	// the agent tab's name from the SAME persisted record, so it agrees even when the
	// independently-minted ids do not. The pane layer is deliberately blind to this
	// heal — paneTabKeys keys the agent slot by name, so correcting the id does not
	// read as "the tab vanished" and close the pane — while liveBindCandidate keys
	// the live stream ON the id, so the adoption itself re-dials the pane onto the
	// working id. That is the self-heal.
	i.mu.Lock()
	for _, td := range target {
		if td.ID == "" {
			continue
		}
		for idx, t := range i.Tabs {
			if t.Name != td.Name || t.ID == td.ID {
				continue
			}
			// Guard the adopt-over-non-empty to the agent row on BOTH sides, and to
			// index 0 — the position that defines the agent tab here. For every other
			// kind a local id matching no target id is ambiguous (legacy mismatch vs
			// close+recreate), and the drop+add below deliberately owns that case.
			agentRow := idx == 0 && t.Kind == TabKindAgent && td.Kind == TabKindAgent
			if t.ID == "" || agentRow {
				i.replaceTabFieldLocked(idx, func(c *Tab) { c.ID = td.ID })
			}
		}
	}
	// Rename in place by stable id (#1905): a tab whose id is unchanged but whose
	// name changed out-of-band (a rename on another client, #1813) keeps its live
	// tmux session, its slot, and any open pane bound to it — only its name, and
	// so the label derived from it, changes. Without this a rename reads as "old
	// name gone, new name added", which drops the tab and re-adds it at the END
	// of the roster, blipping its PTY and reordering it.
	for _, td := range target {
		if td.ID == "" {
			continue
		}
		for idx, t := range i.Tabs {
			if t.ID == td.ID && t.Name != td.Name {
				i.replaceTabFieldLocked(idx, func(c *Tab) { c.Name = td.Name })
				changed = true
			}
		}
	}
	i.mu.Unlock()

	targetIDs := make(map[string]bool, len(target))
	targetNames := make(map[string]bool, len(target))
	// Names of target rows carrying NO id — a legacy roster written before #1738.
	// Such a row cannot be id-matched, so name is the only key it has, and a local
	// tab it covers must survive on the name alone. Without this the local tab
	// (whose id was minted on add) is dropped and re-added on EVERY poll.
	targetNamesWithoutID := make(map[string]bool, len(target))
	for _, td := range target {
		if td.ID != "" {
			targetIDs[td.ID] = true
		} else {
			targetNamesWithoutID[td.Name] = true
		}
		targetNames[td.Name] = true
	}

	// Drop local non-agent tabs the daemon no longer lists. A tab survives if the
	// daemon still lists its stable id, or — only when the daemon's row for that
	// name carries no id at all — its name. No kill: the daemon owns the teardown
	// and already closed the tmux session (#960 PR 3).
	i.mu.RLock()
	var dropIDs, dropNames []string
	for idx := 1; idx < len(i.Tabs); idx++ {
		switch t := i.Tabs[idx]; {
		case t.ID != "":
			if !targetIDs[t.ID] && !targetNamesWithoutID[t.Name] {
				dropIDs = append(dropIDs, t.ID)
			}
		case !targetNames[t.Name]:
			dropNames = append(dropNames, t.Name)
		}
	}
	i.mu.RUnlock()
	for _, id := range dropIDs {
		if i.dropTabByID(id) {
			changed = true
		}
	}
	for _, name := range dropNames {
		if i.dropTabByName(name) {
			changed = true
		}
	}

	// Snapshot what survived: the add pass keys "already present" on the id, so it
	// must be read AFTER the drops above (a close+recreate frees the reused name
	// there, and only then may the new id be added).
	i.mu.RLock()
	localIDs := make(map[string]bool, len(i.Tabs))
	localNames := make(map[string]bool, len(i.Tabs))
	for _, t := range i.Tabs {
		if t.ID != "" {
			localIDs[t.ID] = true
		}
		localNames[t.Name] = true
	}
	i.mu.RUnlock()

	// Add daemon-listed tabs missing locally, reconnecting each to its exact
	// persisted tmux session by name so it is immediately attachable.
	var firstErr error
	for _, td := range target {
		if td.Kind == TabKindAgent {
			continue
		}
		if td.ID != "" {
			if localIDs[td.ID] {
				continue
			}
		} else if localNames[td.Name] {
			continue // legacy roster row: the name is the only key it has
		}
		kind := tabKindForData(td.Kind)
		// A tmux-less kind (web, vscode) is materialized by the append alone — there
		// is no session to reconnect, exactly as restoreLocalTabs builds it on load.
		// Skipping it on its empty TmuxName (as this loop once did) read "" as a
		// missing session rather than a kind that never has one, so a web/vscode tab
		// created out-of-band stayed invisible in a running TUI until a full rebuild
		// — even though #1815 now delivers the roster that carries it, and even though
		// the DROP side above already removed such a tab by name. See TabKind.HasTmux.
		var ts *tmux.TmuxSession
		if kind.HasTmux() {
			if td.TmuxName == "" || worktreePath == "" {
				continue
			}
			// The sibling inherits the agent session's PTY factory / executor (real
			// in production, mock in tests), binding to the EXACT persisted name.
			// ATTACH-ONLY: pass empty workDir so a missing session errors instead of
			// re-spawning (#1152). Like AttachShellTab, this is a pure TUI-side
			// projection of daemon-owned tabs; the daemon is the single writer that
			// owns every spawn (#960). If the daemon killed the session in the race
			// window, re-spawning here would orphan a tmux session over the deleted
			// worktree. Skip the tab on failure and let the next snapshot reconcile it.
			ts = agentTmux.NewSiblingSession(td.TmuxName, tabProgram(kind, td.Command, program))
			if err := ts.Restore(""); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("failed to reconnect tab %q: %w", td.Name, err)
				}
				continue
			}
		}
		id := td.ID
		if id == "" {
			id = newTabID()
		}
		// URL rides along for a web tab (a vscode tab has none by design — its target
		// is resolved at proxy time), or the pane would have nothing to iframe.
		tab := &Tab{ID: id, Name: td.Name, Kind: kind, Command: td.Command, URL: td.URL, tmux: ts}
		// Adopt under the write lock, re-checking BOTH the already-present dedupe (a
		// concurrent reconcile/AddTab may have added this tab while we reconnected
		// outside the lock) and the teardown fence a Kill/archive can have raised in
		// that same window (#2100). See appendReconciledTab.
		if i.appendReconciledTab(td.ID, td.Name, tab) {
			changed = true
		}
	}

	// Reorder LAST, once the local set matches the daemon's. A pure reorder leaves
	// every id and name unchanged, so every pass above is a no-op for it and the
	// order would never reach a running TUI until restart (#1813). Permuting to the
	// daemon's authoritative order here is what closes that gap.
	if i.reorderTabsFromData(target) {
		changed = true
	}

	return changed, firstErr
}

// dropTabByName removes the named non-agent tab from the in-memory list WITHOUT
// killing its tmux session — the no-kill counterpart of CloseTab used by
// ReconcileTabsFromData when the daemon has already torn the session down (#960
// PR 3). Returns whether a tab was removed. The agent tab (index 0) is never
// dropped.
func (i *Instance) dropTabByName(name string) bool {
	return i.dropTabWhere(func(t *Tab) bool { return t.Name == name }, "name "+name)
}

// dropTabByID is dropTabByName keyed on the stable id (#1738) — what the
// id-keyed snapshot reconcile drops on, so a tab whose id left the daemon's
// roster goes even when a NEW tab has already reused its name (#1886).
func (i *Instance) dropTabByID(id string) bool {
	if id == "" {
		return false
	}
	return i.dropTabWhere(func(t *Tab) bool { return t.ID == id }, "id "+id)
}

// dropTabWhere removes the first non-agent tab matching pred and releases its
// TUI-side attach PTY. label names the match for the log line only.
func (i *Instance) dropTabWhere(pred func(*Tab) bool, label string) bool {
	i.mu.Lock()
	var dropped *Tab
	for idx := 1; idx < len(i.Tabs); idx++ {
		if pred(i.Tabs[idx]) {
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
			log.WarningLog.Printf("dropTab (%s): releasing attach client: %v", label, err)
		}
	}
	return true
}

// replaceTabFieldLocked swaps the tab at idx for a COPY carrying the mutation f
// applies, instead of writing the field in place. Callers must hold i.mu for
// writing.
//
// GetTabs copies only the SLICE and hands out the same *Tab pointers, which
// callers (tree.TabLabels on the render path, the pane refresh) then read
// without holding i.mu — so assigning to a live tab's Name/ID races those
// readers. Copy-on-write keeps the readers race-free: a reader that already
// holds the old pointer keeps reading a consistent old value, and the next
// GetTabs hands out the new one. The tmux pointer rides along on the copy, so
// the tab's live session is preserved across the swap (no PTY blip).
func (i *Instance) replaceTabFieldLocked(idx int, f func(*Tab)) {
	cp := *i.Tabs[idx]
	f(&cp)
	i.Tabs[idx] = &cp
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
