package app

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// prInfoStaleAfter is how long a fetched PR info entry is considered fresh.
// Selection changes within this window do not re-trigger a fetch.
const prInfoStaleAfter = 60 * time.Second

// -- Ticker message types --

type tickUpdateMetadataMessage struct{}
type tickUpdatePRInfoMessage struct{}
type tickPendingInstancesMessage struct{}
type tickRefreshExternalMessage struct{}

// snapshotFetchedMsg carries the result of an off-loop daemon Snapshot fetch
// back to the event loop, where reconcileSnapshot mutates the sidebar (sidebar
// mutation must stay on the bubbletea loop — #682). err is non-nil on a failed
// fetch; the handler retries (daemon warming) or falls back to the disk refresh
// (version-skewed daemon without the Snapshot RPC).
type snapshotFetchedMsg struct {
	data []session.InstanceData
	err  error
}

// prInfoUpdatedMsg is returned by an async PR info fetch.
// info is nil when the fetch resolved to "no PR for this branch".
// err is set for transient errors — the handler keeps the cached value.
// branch is the worktree branch captured at fetch kickoff (the same value
// passed to prInfoFetcher); the handler drops the update if the re-resolved
// target instance is no longer on that branch (#921).
type prInfoUpdatedMsg struct {
	instance *session.Instance
	branch   string
	info     *git.PRInfo
	err      error
}

// -- Ticker commands --
// Each ticker sleeps for a fixed interval, then returns its message type to
// re-enter Update(). The ticker is re-scheduled at the end of its handler.

var tickUpdateMetadataCmd = func() tea.Msg {
	time.Sleep(500 * time.Millisecond)
	return tickUpdateMetadataMessage{}
}

// runMetadataTick refreshes each started instance's status/prompt state by
// shelling out to tmux capture-pane via CheckAndHandleTrustPrompt and
// HasUpdated. Intended to run off the bubbletea Update goroutine (see
// runMetadataTickCmd) so the per-instance tmux work does not block rendering.
// Status mutations go through Instance.SetStatus, which holds the instance
// mutex, so concurrent reads from the renderer remain safe.
func runMetadataTick(instances []*session.Instance) {
	tickStart := time.Now()
	detachTraceMark("runMetadataTick-entry")
	for _, instance := range instances {
		// Deleting instances are skipped like Loading ones: their backing
		// session is being torn down, so the capture-pane/status shell-outs
		// would poke a dying session, and a status write would clobber the
		// Deleting marker (#844).
		if status := instance.GetStatus(); !instance.Started() || status == session.Loading || status == session.Deleting {
			continue
		}
		instStart := time.Now()
		instance.CheckAndHandleTrustPrompt()
		updated, prompt := instance.HasUpdated()
		// SetStatusIfNotDeleting, not SetStatus: the user can confirm a kill
		// between this tick's status check above and the write below, and an
		// unconditional write would un-mark the deleting row (#844).
		if updated {
			instance.SetStatusIfNotDeleting(session.Running)
		} else {
			if prompt {
				instance.TapEnter()
			} else if !instance.TmuxAlive() {
				// HasUpdated returned (false,false), which a healthy idle
				// session and a dead one (monitor.dead / ErrSessionGone) both
				// produce — indistinguishable on their own. Probe liveness only
				// on this idle branch (not every tick) so a vanished session is
				// marked Dead and rendered distinctly rather than repainted as a
				// green Ready dot it can no longer back (#935).
				instance.SetStatusIfNotDeleting(session.Dead)
			} else {
				instance.SetStatusIfNotDeleting(session.Ready)
			}
		}
		// Per-instance elapsed makes contention visible: if 1 of N tmux
		// capture-pane shell-outs hangs, the marker for that instance
		// will dominate the total while the others stay sub-10ms.
		detachTraceFields(instStart, "runMetadataTick-instance-done",
			fmt.Sprintf("title=%s", instance.Title))
	}
	detachTrace(tickStart, "runMetadataTick-exit")
}

// runMetadataTickCmd returns a tea.Cmd that performs the metadata tick work
// for the supplied snapshot of instances off the event loop, then sleeps for
// 500ms before re-emitting tickUpdateMetadataMessage. The sleep happens after
// the work so two ticks can never overlap on the same tmux sessions.
func runMetadataTickCmd(instances []*session.Instance) tea.Cmd {
	return func() tea.Msg {
		runMetadataTick(instances)
		time.Sleep(500 * time.Millisecond)
		return tickUpdateMetadataMessage{}
	}
}

var tickUpdatePRInfoCmd = func() tea.Msg {
	time.Sleep(60 * time.Second)
	return tickUpdatePRInfoMessage{}
}

// tickPendingInstancesCmd processes one-shot pending instances written by
// scheduled task runs (cleared after reading). Runs every 5s.
var tickPendingInstancesCmd = func() tea.Msg {
	time.Sleep(5 * time.Second)
	return tickPendingInstancesMessage{}
}

// snapshotRefreshInterval is how often the TUI polls the daemon for the
// authoritative session snapshot and reconciles its sidebar to it (#960 PR 3).
// Tightened from the old 3s disk poll toward the ~500ms–1s the single-writer
// design approved, so an out-of-band tab/session appears near-instantly. The
// sleep is the gap BETWEEN reconciles (it precedes each tick), so a slow fetch
// extends the period rather than overlapping the next one.
const snapshotRefreshInterval = 750 * time.Millisecond

// tickRefreshExternalCmd paces the snapshot reconcile loop: it sleeps one
// interval, then emits tickRefreshExternalMessage, whose handler kicks off an
// off-loop Snapshot fetch (fetchSnapshotCmd). The loop re-arms from the
// snapshotFetchedMsg handler after each reconcile, so only one fetch is ever in
// flight.
var tickRefreshExternalCmd = func() tea.Msg {
	time.Sleep(snapshotRefreshInterval)
	return tickRefreshExternalMessage{}
}

// fetchSnapshotCmd fetches the daemon's authoritative session list off the event
// loop (the RPC may briefly block while a daemon warms up — #829) and returns it
// as a snapshotFetchedMsg for on-loop reconciliation. Mirrors runMetadataTickCmd:
// the work runs in the tea.Cmd goroutine, the mutation happens in the handler.
func (m *home) fetchSnapshotCmd() tea.Cmd {
	// Capture both the repo and the fetcher on the event loop, BEFORE the goroutine
	// runs: the fetcher is a per-home field (not a shared global), and reading it
	// here rather than inside the closure keeps the off-loop goroutine free of any
	// field access that could race a concurrent reassignment (#960 PR 4 race fix).
	repoID := m.repoID
	fetch := m.snapshotFetcher
	return func() tea.Msg {
		data, err := fetch(repoID)
		return snapshotFetchedMsg{data: data, err: err}
	}
}

// prInfoFetcher is the function used by fetchPRInfoCmd to retrieve PR info.
// It's a package-level variable (not a direct git.FetchPRInfo call) so
// e2e tests can swap in a fake that returns canned data and counts calls —
// the real fetcher shells out to `gh`, which we don't want in unit tests.
var prInfoFetcher = git.FetchPRInfo

// SetPRInfoFetcherForTest replaces prInfoFetcher with f and returns a
// restore function. Test-only.
func SetPRInfoFetcherForTest(f func(repoPath, branch string) (*git.PRInfo, error)) func() {
	prev := prInfoFetcher
	prInfoFetcher = f
	return func() { prInfoFetcher = prev }
}

// fetchPRInfoCmd returns a tea.Cmd that fetches PR info for inst in a
// background goroutine and emits a prInfoUpdatedMsg. Returns nil when the
// instance is not eligible for a fetch (nil / not started / remote / already
// fresh / fetch already in flight). Using force=true ignores the freshness
// check — for tick-driven refreshes of the selected instance.
func fetchPRInfoCmd(inst *session.Instance, force bool) tea.Cmd {
	if inst == nil || inst.IsRemote() {
		return nil
	}
	if !force && inst.PRInfoAge() < prInfoStaleAfter {
		return nil
	}
	repoPath, branch := inst.FetchPRInfoSnapshot()
	// An empty branch means a detached-HEAD worktree: there is no branch to
	// look up, so skip the fetch entirely rather than spawning `gh pr view ""`
	// on every tick. FetchPRInfo defends against this too, but stopping here
	// avoids the goroutine and subprocess churn for the selected instance.
	if repoPath == "" || branch == "" {
		return nil
	}
	// Mark as fetched at kickoff so concurrent callers observe the debounce
	// window while this fetch is in flight. selectionChanged is re-entered
	// every 100ms by the preview tick; without this, restored instances
	// (whose prInfoLastFetched is zero until the first fetch completes)
	// would spawn a new `gh pr view` subprocess on every tick until one
	// returned. The completion handler bumps the timestamp again with the
	// real result, so the fresh window starts from fetch completion.
	inst.MarkPRInfoFetched()
	// Capture the fetch seam on the event loop, before the goroutine reads it: it
	// is a package var swapped by test seams, so reading it inside the cmd
	// goroutine would race a sibling parallel test's swap (#960 PR 4 race-fix
	// class). Reading it here pins the value for this fetch.
	fetch := prInfoFetcher
	return func() tea.Msg {
		fetchStart := time.Now()
		detachTraceMark("fetchPRInfoCmd-goroutine-entry")
		info, err := fetch(repoPath, branch)
		detachTrace(fetchStart, "fetchPRInfoCmd-prInfoFetcher-returned")
		return prInfoUpdatedMsg{instance: inst, branch: branch, info: info, err: err}
	}
}

// -- Sync methods --

// handleSnapshot applies a fetched daemon snapshot to the sidebar and reports
// whether anything changed (the caller repaints only on a diff). On a fetch
// error it degrades rather than dropping the sidebar: a warming daemon (#829) is
// retried on the next tick (callDaemon already waited out the warm-up window),
// and any other error is logged and skipped, leaving the last-known sidebar
// intact. The Snapshot RPC is the TUI's ONLY sync path (#960 PR 4): the daemon is
// the sole owner/writer of session state, so there is no disk-based reconcile to
// fall back to.
func (m *home) handleSnapshot(msg snapshotFetchedMsg) bool {
	if msg.err != nil {
		if daemon.IsDaemonStartingErr(msg.err) {
			// Daemon still restoring (#829); the cold-start LoadInstances already
			// populated the sidebar. Retry next tick — nothing to reconcile yet.
			return false
		}
		log.WarningLog.Printf("failed to fetch daemon snapshot: %v", msg.err)
		return false
	}
	return m.reconcileSnapshot(msg.data)
}

// reconcileSnapshot mirrors the sidebar to the daemon's authoritative snapshot
// (#960 PR 3). The daemon is the single owner of session/tab state, so the TUI
// renders a projection of it:
//   - sessions in the snapshot but missing locally are built (FromInstanceData,
//     reconnecting tabs by tmux name) and added;
//   - sessions gone from the snapshot are removed;
//   - existing rows are updated IN PLACE — same *session.Instance pointer, only
//     its tab list and PR info mutated — which is the #959 "live display" fix
//     (an out-of-band tab now appears without a restart);
//   - a same-title row whose identity (CreatedAt) differs from the snapshot is a
//     kill+recreate of the title (#765); it is swapped for a freshly built
//     instance so the dead corpse never shadows the live session.
//
// Local-only view state is preserved: existing rows keep their pointer (so the
// TabbedWindow's active tab and the content pane's scroll/overlay are untouched),
// and the selected instance is re-pinned by pointer after the reconcile so a
// removal of a preceding row can't drift the cursor. Transient TUI-owned rows
// (Loading creation #808, Deleting kill #844) are left entirely alone: the
// daemon may not yet know about an in-flight create, and a mid-teardown row must
// keep its marker. Returns whether anything changed.
//
// Because the snapshot IS the truth, this is the TUI's ONLY sync path (#960 PR 4
// deleted the disk-based refresh and its dual-writer collision heuristics — the
// #765/#808 "is my in-memory row staler than disk" guessing game that existed
// only to defend a competing TUI writer). A pure mirror just needs an identity
// check (CreatedAt) to tell "same session" from "title reused".
//
// STATUS is deliberately NOT mirrored onto existing rows here: the daemon does
// not yet compute Ready/Dead (that moves to it in #960 PR 5), so its persisted
// status is stale, and clobbering the locally-computed status would fight
// runMetadataTick and flicker the dot. New rows still seed their status from the
// snapshot (FromInstanceData carries it); full status mirroring lands in PR 5.
func (m *home) reconcileSnapshot(data []session.InstanceData) bool {
	snapByTitle := make(map[string]session.InstanceData, len(data))
	for _, d := range data {
		snapByTitle[d.Title] = d
	}

	existing := make(map[string]*session.Instance, len(data))
	for _, inst := range m.sidebar.GetInstances() {
		existing[inst.Title] = inst
	}

	// Capture the selected instance so we can re-pin it after removals shift
	// indices — selection must never drift because a snapshot arrived.
	selected := m.sidebar.GetSelectedInstance()

	changed := false

	for _, d := range data {
		inst := existing[d.Title]
		if inst == nil {
			if m.addInstanceFromSnapshot(d) {
				changed = true
			}
			continue
		}
		// In-flight TUI operations own their row: leave Loading/Deleting alone.
		if isTransientStatus(inst.GetStatus()) {
			continue
		}
		if !inst.CreatedAt.Equal(d.CreatedAt) {
			// Same title, different session — a kill+recreate reused the title
			// (#765). Swap the stale row for the live one rather than mutating the
			// corpse in place.
			if m.swapInstanceFromSnapshot(d) {
				changed = true
			}
			continue
		}
		if m.updateInstanceFromSnapshot(inst, d) {
			changed = true
		}
	}

	// Remove sessions the daemon no longer owns. Skip transient rows (an
	// in-flight create the daemon doesn't list yet; a mid-teardown kill).
	var toRemove []*session.Instance
	for _, inst := range m.sidebar.GetInstances() {
		if _, ok := snapByTitle[inst.Title]; ok {
			continue
		}
		if isTransientStatus(inst.GetStatus()) {
			continue
		}
		toRemove = append(toRemove, inst)
	}
	for _, inst := range toRemove {
		m.sidebar.RemoveInstanceByTitle(inst.Title)
		changed = true
	}

	// Re-pin the selection to the same instance if it survived the reconcile.
	if selected != nil && m.sidebar.ContainsInstance(selected) {
		m.sidebar.SelectInstance(selected)
	}

	return changed
}

// addInstanceFromSnapshot builds a live instance from a snapshot record and adds
// it to the sidebar. Returns true on success (a real change). A build failure is
// logged and skipped — a single unrestorable record must not abort the whole
// reconcile.
func (m *home) addInstanceFromSnapshot(d session.InstanceData) bool {
	inst, err := buildInstanceFromSnapshot(d)
	if err != nil {
		log.WarningLog.Printf("failed to build instance %q from snapshot: %v", d.Title, err)
		return false
	}
	m.sidebar.AddInstance(inst)()
	inst.SetAutoYes(m.autoYes)
	return true
}

// swapInstanceFromSnapshot replaces a stale same-title sidebar row with a freshly
// built instance for the recreated session (#765), preserving the selected row.
// Built BEFORE the swap so a transient build failure leaves the existing row in
// place rather than dropping it.
func (m *home) swapInstanceFromSnapshot(d session.InstanceData) bool {
	inst, err := buildInstanceFromSnapshot(d)
	if err != nil {
		log.WarningLog.Printf("failed to build recreated instance %q from snapshot: %v", d.Title, err)
		return false
	}
	if !m.sidebar.ReplaceInstanceByTitle(d.Title, inst) {
		// The row vanished between read and swap; add it fresh.
		m.sidebar.AddInstance(inst)()
	}
	inst.SetAutoYes(m.autoYes)
	return true
}

// updateInstanceFromSnapshot reconciles an existing sidebar row's tab list and
// PR badge to the snapshot IN PLACE (same pointer, so view state survives).
// Returns whether anything changed.
func (m *home) updateInstanceFromSnapshot(inst *session.Instance, d session.InstanceData) bool {
	changed := false
	// Remote instances' tabs come from hook config (terminal_cmd), not the
	// snapshot, so the backend owns them — skip the tab reconcile.
	if !inst.IsRemote() {
		if tabsChanged, err := inst.ReconcileTabsFromData(d.Tabs); err != nil {
			log.WarningLog.Printf("failed to reconcile tabs for %q from snapshot: %v", d.Title, err)
			changed = changed || tabsChanged
		} else if tabsChanged {
			changed = true
		}
	}
	// PR info mirrors the daemon's recorded value. This runs on the event loop,
	// serialized with prInfoUpdatedMsg, so it never races the TUI's own
	// fetch-then-write of the same badge.
	if prInfoDiffersFromData(inst, d.PRInfo) {
		inst.SetPRInfo(prInfoFromData(d.PRInfo))
		changed = true
	}
	return changed
}

// prInfoFromData rebuilds a *git.PRInfo from its serialized form, returning nil
// for the zero value (Number 0 = "no PR"), matching FromInstanceData.
func prInfoFromData(d session.PRInfoData) *git.PRInfo {
	if d.Number == 0 {
		return nil
	}
	return &git.PRInfo{Number: d.Number, Title: d.Title, URL: d.URL, State: d.State}
}

// prInfoDiffersFromData reports whether an instance's in-memory PR info differs
// from the snapshot's, so the reconcile only writes (and reports a change) on an
// actual diff.
func prInfoDiffersFromData(inst *session.Instance, d session.PRInfoData) bool {
	cur := inst.GetPRInfo()
	if d.Number == 0 {
		return cur != nil
	}
	if cur == nil {
		return true
	}
	return cur.Number != d.Number || cur.Title != d.Title || cur.URL != d.URL || cur.State != d.State
}

// isTransientStatus reports whether an in-memory sidebar instance is in a
// state owned by an in-flight TUI operation — Loading (creation, #808) or
// Deleting (async kill, #844) — during which the snapshot reconcile must
// neither replace nor reap it.
func isTransientStatus(status session.Status) bool {
	return status == session.Loading || status == session.Deleting
}

func (m *home) importRemoteHookSessions() int {
	repo, err := config.CurrentRepo()
	if err != nil {
		log.WarningLog.Printf("failed to resolve repo for remote import: %v", err)
		return 0
	}
	repoCfg, err := config.ResolveConfig(repo.Root)
	if err != nil {
		log.WarningLog.Printf("failed to resolve repo config for remote import: %v", err)
		return 0
	}
	if repoCfg.RemoteHooks == nil || repoCfg.RemoteHooks.ListCmd == "" {
		return 0
	}

	listed, err := importRemoteSessionsThroughDaemon(repo.Root)
	if err != nil {
		if daemon.IsDaemonStartingErr(err) {
			// The daemon is up but still restoring instances (#829); not a
			// failure. Already-persisted remote sessions were loaded from
			// storage above, and newly-discovered ones import on the next
			// TUI launch once the daemon is warm.
			log.InfoLog.Printf("daemon still restoring instances; skipping remote hook import this launch")
			return 0
		}
		log.WarningLog.Printf("failed to list remote hook sessions: %v", err)
		return 0
	}

	existingTitles := m.sidebar.GetInstanceTitles()
	existingHookNames := make(map[string]bool)
	for _, inst := range m.sidebar.GetInstances() {
		if !inst.IsRemote() {
			continue
		}
		data := inst.ToInstanceData()
		existingHookNames[session.RemoteHookName(data.Title, data.RemoteMeta)] = true
	}

	imported := 0
	for _, data := range listed {
		name := session.RemoteHookName(data.Title, data.RemoteMeta)
		if existingTitles[data.Title] || existingHookNames[name] {
			continue
		}

		inst, err := session.FromInstanceData(data)
		if err != nil {
			log.WarningLog.Printf("failed to import remote hook session %q: %v", data.Title, err)
			continue
		}
		m.sidebar.AddInstance(inst)()
		inst.SetAutoYes(m.autoYes)
		existingTitles[data.Title] = true
		existingHookNames[name] = true
		imported++
	}

	return imported
}
