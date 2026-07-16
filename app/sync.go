package app

import (
	"fmt"
	"reflect"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
)

// prInfoStaleAfter is how long a fetched PR info entry is considered fresh.
// Selection changes within this window do not re-trigger a fetch.
const prInfoStaleAfter = 60 * time.Second

// -- Ticker message types --

type tickUpdatePRInfoMessage struct{}
type tickRefreshExternalMessage struct{}

// snapshotFetchedMsg carries the result of an off-loop daemon Snapshot fetch
// back to the event loop, where reconcileSnapshot mutates the projection store
// (store mutation must stay on the bubbletea loop — #682). err is non-nil on a failed
// fetch; the handler retries (daemon warming) or falls back to the disk refresh
// (version-skewed daemon without the Snapshot RPC).
type snapshotFetchedMsg struct {
	data []session.InstanceData
	err  error
	// repoID is the repo the fetch was scoped to, captured at spawn. The handler
	// drops the whole message when it no longer matches m.repoID: an in-place
	// project switch (#1461) can change the active repo while a fetch launched
	// for the previous repo is still in flight, and applying its old-repo
	// sessions/tasks would bleed the previous project into the new view.
	repoID string
	// tasks is the repo's task list re-read from disk on the same poll (#1168).
	// Tasks are a disk-backed store shared between the TUI and the daemon — not
	// solely daemon-owned like sessions (#960) — so an out-of-band `af tasks
	// add|update|remove` is picked up here and live-projected into the rail/
	// overlay instead of only appearing after a relaunch. Carried alongside the
	// session snapshot so both external changes ride one 750ms poll. tasksErr is
	// independent of err: a warming daemon can fail the session RPC while the
	// disk task read still succeeds.
	tasks    []task.Task
	tasksErr error
	// alarms carries any persistent watch-task delivery-failure alarms projected
	// on the same daemon snapshot (#1238). It rides the session snapshot because
	// it is a field of the one authoritative Snapshot response, not a separate
	// subsystem; handleSnapshot mirrors it into the alarm banner state.
	alarms []daemon.DeliveryAlarm
	// allRepos is the daemon's cross-repo session list (RepoID:"") fetched on the
	// same poll, off-loop, so the always-visible bottom Projects section can
	// refresh its per-repo counts live when sessions change in ANOTHER repo
	// (#1590). allReposErr is carried separately (like tasksErr) so a failed
	// cross-repo read leaves the last-known project rows intact rather than
	// blanking the section on a hiccup.
	allRepos    []session.InstanceData
	allReposErr error
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
	// repoID is the repo the fetch was scoped to, captured at kickoff. The
	// handler drops the message when it no longer matches m.repoID: an in-place
	// project switch (#1461) resets the store and swaps the active repo while a
	// fetch launched for the previous one is still in flight, and the handler's
	// title-only re-resolution would otherwise land the old project's PR info on
	// a same-title, same-branch session in the NEW project — then persist it
	// under the new repoID, where the snapshot reconcile mirrors it back and
	// makes the bleed durable (#1780). Mirrors snapshotFetchedMsg's guard.
	repoID string
	info   *git.PRInfo
	err    error
}

// -- Ticker commands --
// Each ticker sleeps for a fixed interval, then returns its message type to
// re-enter Update(). The ticker is re-scheduled at the end of its handler.

var tickUpdatePRInfoCmd = func() tea.Msg {
	time.Sleep(60 * time.Second)
	return tickUpdatePRInfoMessage{}
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
	repoRoot := m.repoRoot
	fetch := m.snapshotFetcher
	allRepos := allReposSnapshotFetcher
	return func() tea.Msg {
		resp, err := fetch(repoID)
		// Re-read the ACTIVE repo's tasks on the same poll so an out-of-band task
		// change live-projects into the TUI (#1168). Scoped to the captured
		// repoRoot, not cwd, so a poll after an in-place project switch (#1461)
		// reads the switched-to project's tasks — not the launch repo's. Its own
		// error is carried separately so a warming-daemon RPC failure never
		// suppresses a successful disk task read.
		tasks, tasksErr := task.LoadTasksForRepo(repoRoot)
		// Fetch the cross-repo session list on the same off-loop poll so the
		// always-visible Projects section's per-repo counts stay live (#1590).
		// Read here (not in the handler) to keep the daemon RPC off the event
		// loop; its error is carried separately so a hiccup keeps the last rows.
		allReposData, allReposErr := allRepos()
		return snapshotFetchedMsg{
			data: resp.Instances, alarms: resp.DeliveryAlarms, err: err,
			tasks: tasks, tasksErr: tasksErr, repoID: repoID,
			allRepos: allReposData, allReposErr: allReposErr,
		}
	}
}

// coldStartWarmupPoll paces the cold-start Snapshot retry while the daemon is
// still restoring instances (#829). A package var so the warm-up retry path is
// driven deterministically in tests without real sleeps.
var coldStartWarmupPoll = 250 * time.Millisecond

// coldStartWarmupWait bounds total cold-start waiting on a warming daemon. A
// local restore completes in well under a second; the generous ceiling covers a
// minutes-long remote-hook restore (#829) without hanging the launch forever if
// the daemon is wedged reporting "starting".
var coldStartWarmupWait = 2 * time.Minute

// coldStartFromSnapshot populates the projection store from the daemon's authoritative
// Snapshot at startup, replacing the legacy instances.json disk read (#960 PR 6:
// the TUI no longer reads the store — the daemon is the source of truth). Each
// record is materialized the same way FromInstanceData restored a disk row,
// reconnecting tabs to their tmux sessions by name so a restored session is
// immediately attachable. A single unrestorable record is logged and skipped,
// never aborting the whole cold start. Returns an error only on a hard
// (non-warming) daemon failure, which newHome surfaces and exits on — there is
// no standalone fallback anymore (#960 PR 6 dropped no-daemon mode).
func (m *home) coldStartFromSnapshot() error {
	data, err := m.fetchColdStartSnapshot(m.repoID)
	if err != nil {
		return err
	}
	m.materializeSnapshot(data)
	return nil
}

// materializeSnapshot builds each snapshot record into a live instance and adds
// it to the projection. Split out of coldStartFromSnapshot so switchProject can
// fetch a target project's snapshot up front — while the outgoing project is
// still intact — and only materialize it once the fetch has succeeded (#1788).
// A single unrestorable record is logged and skipped, never aborting the batch.
func (m *home) materializeSnapshot(data []session.InstanceData) {
	for _, d := range data {
		inst, err := buildInstanceFromSnapshot(d)
		if err != nil {
			// The session's tmux/worktree may have been destroyed externally;
			// log and skip rather than failing the whole launch.
			log.WarningLog.Printf("skipping session %q from snapshot: %v", d.Title, err)
			continue
		}
		m.store.AddInstance(inst)()
		inst.SetAutoYes(m.autoYes)
	}
}

// fetchColdStartSnapshot fetches the cold-start snapshot, tolerating a warming
// daemon (#829) exactly like create/kill: callDaemon already waits out one
// warm-up window internally, and this retries the whole fetch while the daemon
// reports the typed "starting" error so a minutes-long restore yields the real
// session list rather than an empty sidebar that looks like a fresh install
// (#766/#868). On a hard error the daemon is unavailable and the launch aborts —
// post-#782 the daemon is always-on, so there is no degraded standalone path.
// The repoID is passed explicitly rather than read from m.repoID so switchProject
// can fetch the INCOMING project's snapshot before it re-scopes the model (#1788).
func (m *home) fetchColdStartSnapshot(repoID string) ([]session.InstanceData, error) {
	deadline := time.Now().Add(coldStartWarmupWait)
	announced := false
	for {
		resp, err := m.snapshotFetcher(repoID)
		if err == nil {
			return resp.Instances, nil
		}
		if !daemon.IsDaemonStartingErr(err) {
			return nil, err
		}
		if !announced {
			// Pre-TUI stdout, matching newHome's other startup messages: tell the
			// user we are waiting on the daemon rather than showing an empty list.
			fmt.Println("Restoring sessions… (daemon warming up)")
			announced = true
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemon is still restoring sessions after %s; try again in a moment", coldStartWarmupWait)
		}
		time.Sleep(coldStartWarmupPoll)
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
// check — for tick-driven refreshes of the selected instance. repoID is the
// caller's active repo, read on the event loop and carried on the result so the
// handler can drop a fetch that outlived a project switch (#1780).
func fetchPRInfoCmd(inst *session.Instance, repoID string, force bool) tea.Cmd {
	// PR info comes from a local git branch (`gh pr view`), so it only applies to
	// a backend with a local worktree.
	if inst == nil || inst.Capabilities().Workspace != session.WorkspaceLocalWorktree {
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
		return prInfoUpdatedMsg{instance: inst, branch: branch, repoID: repoID, info: info, err: err}
	}
}

// -- Sync methods --

// handleSnapshot applies a fetched daemon snapshot to the projection store and reports
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
			// Daemon still restoring (#829); the cold-start Snapshot already
			// populated the sidebar. Retry next tick — nothing to reconcile yet.
			return false
		}
		log.WarningLog.Printf("failed to fetch daemon snapshot: %v", msg.err)
		return false
	}
	changed := m.reconcileSnapshot(msg.data)
	if m.applyDeliveryAlarms(msg.alarms) {
		changed = true
	}
	return changed
}

// applyDeliveryAlarms mirrors the snapshot's persistent delivery-failure alarms
// (#1238) into the banner. Because the banner reserves a layout row exactly
// when it is active, an active-state flip re-solves the grid so the row appears
// or disappears with the alarm. Returns whether the visible alarm set changed
// (the caller repaints on a diff). Only ever called on a successful snapshot:
// a transient RPC error leaves the last-known alarm up rather than clearing a
// real outage on a hiccup.
func (m *home) applyDeliveryAlarms(alarms []daemon.DeliveryAlarm) bool {
	var infos []ui.AlarmInfo
	for _, a := range alarms {
		infos = append(infos, ui.AlarmInfo{
			TaskName: a.TaskName,
			Target:   a.TargetSession,
			Pending:  a.Pending,
			Since:    a.Since,
		})
	}
	// Leave infos nil (not an empty slice) when there are no alarms so the
	// steady-state DeepEqual against a nil last-known set reports "no change".
	wasActive := m.alarmBanner.Active()
	changed := !reflect.DeepEqual(m.alarmBanner.Alarms(), infos)
	m.alarmBanner.SetAlarms(infos)
	if m.alarmBanner.Active() != wasActive {
		// The reserved banner row appeared or vanished — re-rect every region.
		m.relayout()
	}
	return changed
}

// refreshTasks mirrors an out-of-band tasks.json change (a CLI/daemon `af tasks
// add|update|remove`, or a run that bumped a task's last-run status) into the
// running TUI, closing the live-projection gap that left a CLI-created task
// invisible until relaunch (#1168 — the tasks sibling of the #959 tab fix).
// Tasks are a disk-backed store shared between the TUI and the daemon, so the
// TUI re-reads them on the snapshot poll rather than through the daemon's
// session Snapshot. The automations rail (store) always mirrors the fresh list;
// the tasks overlay pane is re-synced only when the user is not mid-edit, so a
// background refresh can never clobber an in-progress create/edit or unsaved
// deletions. Returns whether anything visible changed (the caller repaints on a
// diff). A read error leaves the last-known list intact, matching handleSnapshot.
func (m *home) refreshTasks(tasks []task.Task, tasksErr error) bool {
	if tasksErr != nil {
		log.WarningLog.Printf("failed to refresh tasks: %v", tasksErr)
		return false
	}
	changed := false
	if !reflect.DeepEqual(m.store.GetTasks(), tasks) {
		m.store.SetTasks(tasks)
		changed = true
	}
	// The overlay pane owns transient edit state (create/edit buffers, pending
	// deletions): only re-sync it while idle so a background refresh never wipes
	// the user's in-flight form or unsaved deletes.
	sp := m.automations.TaskPane()
	if !sp.IsEditing() && !sp.IsCreating() && !sp.IsDirty() {
		if !reflect.DeepEqual(sp.GetTasks(), tasks) {
			sp.SetTasks(tasks)
			changed = true
		}
	}
	return changed
}

// reconcileSnapshot mirrors the projection store to the daemon's authoritative
// snapshot (#960 PR 3). The daemon is the single owner of session/tab state, so
// the TUI renders a projection of it:
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
// active tab and the content pane's scroll/overlay are untouched), and the
// store's selected instance is re-pinned by title after the reconcile — the
// sidebar re-derives its cursor from that selection, so neither a removal of a
// preceding row nor a same-title swap of the selected row can drift the
// cursor (#969). Transient TUI-owned rows
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
// STATUS (Ready/Dead/Running) is mirrored onto existing rows here (#960 PR 5):
// the daemon's poll loop is now the sole authority that computes the #935
// liveness, so the TUI renders the snapshot's status instead of computing its
// own — the old runMetadataTick is gone. Transient TUI-owned rows
// (Loading/Deleting) are skipped before the per-row update below, so the mirror
// can't clobber an in-flight create or a mid-teardown kill.
func (m *home) reconcileSnapshot(data []session.InstanceData) bool {
	snapByTitle := make(map[string]session.InstanceData, len(data))
	for _, d := range data {
		snapByTitle[d.Title] = d
	}

	existing := make(map[string]*session.Instance, len(data))
	for _, inst := range m.store.GetInstances() {
		existing[inst.Title] = inst
	}

	// Capture the selected instance's STABLE identity (title) so we can re-pin it
	// after removals shift indices — selection must never drift because a snapshot
	// arrived. Capturing the pointer would go stale when the selected row is itself
	// swapped (same title, different CreatedAt) in this same cycle: swapInstanceFromSnapshot
	// rebuilds a brand-new *session.Instance, orphaning the captured pointer, so a
	// pointer-equality re-pin would miss it and selection would drift (#969). The
	// title survives the swap because the rebuilt instance keeps it — the same
	// title-based re-resolution the async handlers already use (GetInstanceByTitle).
	// The capture reads the sidebar's cursor-derived selection (nil while the
	// cursor rests on a section header), not the store's sticky display
	// binding: a reconcile must only re-pin the cursor when the cursor was
	// actually on an instance row, never yank it off a header.
	var selectedTitle string
	if selected := m.sidebar.GetSelectedInstance(); selected != nil {
		selectedTitle = selected.Title
	}

	changed := false

	for _, d := range data {
		inst := existing[d.Title]
		if inst == nil {
			if m.addInstanceFromSnapshot(d) {
				changed = true
			}
			continue
		}
		if !sameSession(inst, d) {
			// Same title, different session — a kill+recreate reused the title
			// (#765). Swap the stale row for the live one rather than mutating the
			// corpse in place. EXCEPT: a row with a local op in flight (a create
			// placeholder, an optimistic kill) owns its identity transition via its
			// own completion handler (instanceStartedMsg/instanceKilledMsg, which
			// use pointer identity); swapping it here would orphan that handshake
			// and leave two same-title rows (#808). Leave it and let the op resolve.
			if inst.GetInFlightOp() != session.OpNone {
				continue
			}
			if m.swapInstanceFromSnapshot(d) {
				changed = true
			}
			continue
		}
		// A same-session row transitioning OUT of Archived back to a live liveness
		// (the restore case) must be REBUILT, not updated in place.
		// handleInstanceArchived/SetArchived flipped the projection inert
		// (started=false); an in-place SetLiveness(live) would leave it "live but not
		// started", so attach/send-keys/Enter/preview all fail their !started guard
		// and the agent-tmux binding is never re-established (ReconcileTabsFromData
		// no-ops on a not-started row) until a TUI relaunch.
		// buildInstanceFromSnapshot (=FromInstanceData) re-runs Start() for a live
		// liveness, restoring started + the binding IN-PLACE — symmetric to
		// SetArchived flipping it inert (#1195 restore regression). Keyed on the
		// Archived→live transition specifically (not merely !Started), so it fires
		// on real restores without disturbing live in-place updates.
		if inst.GetLiveness() == session.LiveArchived && snapshotLiveness(inst.GetLiveness(), d) != session.LiveArchived {
			if m.swapInstanceFromSnapshot(d) {
				changed = true
			}
			continue
		}
		// Same session: apply the daemon's liveness UNCONDITIONALLY and clear a
		// local op once the liveness confirms its outcome. This is the #1195
		// structural fix for #1187: because liveness is never skipped, a
		// locally-archiving row can no longer be stranded on "Tearing down…" — the
		// terminal Archived always lands, and the op clears. The old
		// isTransientStatus/overrideDeleting apparatus is gone.
		if m.updateInstanceFromSnapshot(inst, d) {
			changed = true
		}
	}

	// Remove sessions the daemon no longer owns. Skip rows with a local op in
	// flight (a create the daemon doesn't list yet; a mid-teardown kill whose
	// record the daemon just deleted) — their completion handler owns the
	// removal, not the reconcile (#808/#844).
	var toRemove []*session.Instance
	for _, inst := range m.store.GetInstances() {
		if _, ok := snapByTitle[inst.Title]; ok {
			continue
		}
		if inst.GetInFlightOp() != session.OpNone {
			continue
		}
		toRemove = append(toRemove, inst)
	}
	for _, inst := range toRemove {
		m.store.RemoveInstanceByTitle(inst.Title)
		changed = true
	}
	// Prune panes whose backing session can no longer render — a removed
	// instance takes its open panes with it (#1088), and a session archived out
	// of band (`af sessions archive` while the TUI runs, #1028) leaves its pane
	// dangling on a torn-down session. Do it here, not just on the next
	// selectionChanged tick, so the focus ring and pane layout are consistent
	// the moment the reconcile returns. Unconditional: pruneDeadPanes is O(open
	// panes) and only relayouts when it actually closed something, so a reconcile
	// with nothing to prune is a cheap no-op.
	if m.pruneDeadPanes() {
		m.relayout()
	}

	// Re-pin the selection to the same logical instance (by title) if it
	// survived the reconcile. Re-resolving by title correctly re-pins across a
	// swap because the rebuilt instance keeps the same title (#969). The store's
	// SelectInstance records the assertion; the sidebar moves its cursor onto
	// the asserted row on its next read. If the selected title is gone from the
	// snapshot, leave the sidebar's own clamp behavior as-is.
	if selectedTitle != "" {
		if inst := m.store.GetInstanceByTitle(selectedTitle); inst != nil {
			m.store.SelectInstance(inst)
		}
	}

	return changed
}

// sameSession reports whether an existing same-title row and a snapshot record
// are the SAME logical session rather than a kill+recreate that reused the title
// (#765). It prefers the stable per-session ID (#1195): when both the row and the
// record carry one, identity is a pure ID compare, immune to the CreatedAt-as-
// identity gotcha the audit flagged (a manufactured or zero-CreatedAt record
// silently degrading a swap into an in-place corpse mutation). Legacy records
// written before #1195 have no ID; for them it falls back to CreatedAt equality —
// exactly the prior behavior — so mixed old/new records reconcile correctly.
func sameSession(inst *session.Instance, d session.InstanceData) bool {
	if inst.ID != "" && d.ID != "" {
		return inst.ID == d.ID
	}
	return inst.CreatedAt.Equal(d.CreatedAt)
}

// addInstanceFromSnapshot builds a live instance from a snapshot record and adds
// it to the projection store. Returns true on success (a real change). A build
// failure is logged and skipped — a single unrestorable record must not abort
// the whole reconcile.
func (m *home) addInstanceFromSnapshot(d session.InstanceData) bool {
	inst, err := buildInstanceFromSnapshot(d)
	if err != nil {
		log.WarningLog.Printf("failed to build instance %q from snapshot: %v", d.Title, err)
		return false
	}
	m.store.AddInstance(inst)()
	inst.SetAutoYes(m.autoYes)
	return true
}

// swapInstanceFromSnapshot replaces a stale same-title row with a freshly
// built instance for the recreated session (#765), preserving the selection.
// Built BEFORE the swap so a transient build failure leaves the existing row in
// place rather than dropping it.
func (m *home) swapInstanceFromSnapshot(d session.InstanceData) bool {
	inst, err := buildInstanceFromSnapshot(d)
	if err != nil {
		log.WarningLog.Printf("failed to build recreated instance %q from snapshot: %v", d.Title, err)
		return false
	}
	// ReplaceInstance re-points any open panes at the rebuilt instance, but
	// the recreated session's tab SET may differ from the corpse's — capture
	// the outgoing slot→name list so the shared pane reconcile can close
	// panes whose tab didn't come back and re-bind ones whose slot moved
	// (#1088).
	var oldNames []string
	if old := m.store.GetInstanceByTitle(d.Title); old != nil {
		oldNames = paneTabNames(old)
	}
	if !m.store.ReplaceInstanceByTitle(d.Title, inst) {
		// The row vanished between read and swap; add it fresh.
		m.store.AddInstance(inst)()
	}
	inst.SetAutoYes(m.autoYes)
	if m.reconcilePanesForTabs(inst, oldNames) {
		m.relayout()
	}
	return true
}

// updateInstanceFromSnapshot reconciles an existing row's tab list and
// PR badge to the snapshot IN PLACE (same pointer, so view state survives).
// Returns whether anything changed.
func (m *home) updateInstanceFromSnapshot(inst *session.Instance, d session.InstanceData) bool {
	changed := false
	// Mirror the daemon's authoritative LIVENESS onto the row (#960 PR 5, #1195):
	// the daemon poll computes Running/Ready/Lost/Archived/LimitReached (the #935
	// liveness) and the TUI renders it. Applied UNCONDITIONALLY — daemon liveness
	// always wins — which is what makes #1187 structurally impossible: a
	// locally-archiving row still receives the terminal Archived instead of being
	// stranded. The op axis is reconciled separately below so a liveness write
	// never clobbers a local optimistic marker by accident.
	lv := snapshotLiveness(inst.GetLiveness(), d)
	if inst.GetLiveness() != lv {
		_ = inst.Transition(session.ObserveLiveness(lv))
		changed = true
	}
	if reconcileSnapshotOp(inst, d.InFlightOp, lv) {
		changed = true
	}
	// Mirror the usage-limit reset time (#1146) alongside the liveness. It's
	// display-only metadata that rides LiveLimitReached — which composes to Ready,
	// so the liveness compare above is what surfaces the [limit] badge; this only
	// keeps its "resets <t>" suffix fresh. Set it on its own axis (not via
	// SetLimitReached) so the liveness stays applied unconditionally above. When
	// the daemon has moved the session off LimitReached, the reset field is left
	// inert: LimitResetAt/ToInstanceData both gate on the liveness, so a stale
	// value can never leak to the badge or to disk.
	if lv == session.LiveLimitReached {
		if curReset, _ := inst.LimitResetAt(); !curReset.Equal(d.LimitResetAt) {
			inst.SetLimitResetAt(d.LimitResetAt)
			changed = true
		}
	}
	// Backends without user-managed tabs (the off-box docker/ssh/hook runtimes)
	// have a tab list their runtime owns — a single agent tab, fixed at Launch —
	// so there is nothing for the snapshot to reconcile against. Skipping is a
	// no-op rather than a behavior change: ReconcileTabsFromData already returns
	// early for an instance with no local worktree, which is every sandbox
	// instance (#1874).
	if inst.Capabilities().TabManagement {
		// Capture the slot→name list before the tab reconcile mutates it: an
		// out-of-band tab removal (another client, `af sessions tab-delete`,
		// daemon-side) must apply the SAME pane close/rebind semantics as the
		// TUI `w` kill, or an open pane is left showing a shifted/stale tab
		// (#1088 + #960 — the daemon is the source of truth for tabs).
		oldNames := paneTabNames(inst)
		tabsChanged, err := inst.ReconcileTabsFromData(d.Tabs)
		if err != nil {
			log.WarningLog.Printf("failed to reconcile tabs for %q from snapshot: %v", d.Title, err)
		}
		if tabsChanged {
			changed = true
			if m.reconcilePanesForTabs(inst, oldNames) {
				m.relayout()
			}
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

// reconcileSnapshotOp mirrors the daemon snapshot's operation axis while
// preserving local optimistic operations until the daemon liveness confirms an
// outcome. Snapshot ops are transient but authoritative for daemon-side
// archive/restore windows (#1436); terminal snapshots carry OpNone and clear
// stale overlays through the Transition chokepoint.
func reconcileSnapshotOp(inst *session.Instance, op session.InFlightOp, lv session.Liveness) bool {
	if op != session.OpNone {
		return adoptSnapshotOp(inst, op, lv)
	}

	switch inst.GetInFlightOp() {
	case session.OpArchiving, session.OpKilling:
		if lv == session.LiveArchived || lv == session.LiveLost {
			_ = inst.Transition(session.ClearOp())
			return true
		}
	case session.OpRestoring:
		if lv != session.LiveArchived {
			_ = inst.Transition(session.ClearOp())
			return true
		}
	}
	return false
}

func adoptSnapshotOp(inst *session.Instance, op session.InFlightOp, lv session.Liveness) bool {
	if inst.GetInFlightOp() == op {
		return false
	}
	if inst.GetInFlightOp() != session.OpNone {
		_ = inst.Transition(session.ClearOp())
	}

	var err error
	switch op {
	case session.OpCreating:
		err = inst.Transition(session.BeginCreate())
	case session.OpKilling:
		err = inst.Transition(session.BeginKill())
	case session.OpArchiving:
		if lv == session.LiveArchived {
			return true
		}
		err = inst.Transition(session.BeginArchive())
	case session.OpRestoring:
		err = inst.Transition(session.MarkRestoring())
	default:
		return false
	}
	if err != nil {
		log.WarningLog.Printf("failed to adopt snapshot op %v for %q: %v", op, inst.Title, err)
		return false
	}
	return true
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

// snapshotLiveness resolves the daemon-owned liveness a snapshot record carries
// (#1195). It prefers the explicit `liveness` field; a record from a daemon that
// predates the field (LivenessUnset) falls back to the legacy `status` int.
//
// A legacy TRANSIENT (Loading/Deleting) carries NO real liveness in the two-axis
// model — the new daemon only ever sends liveness, so a legacy transient is
// version-skew noise from an upgrade window. It must NOT be mapped to Ready
// (LivenessForStatus's default), which would flip a mid-kill row (legacy
// `status: Deleting`) to Ready during the window. Instead we keep the row's
// CURRENT liveness untouched; the daemon reports a real liveness on a later poll
// once it's upgraded. `cur` is the existing row's liveness for exactly this.
func snapshotLiveness(cur session.Liveness, d session.InstanceData) session.Liveness {
	if d.Liveness != session.LivenessUnset {
		return d.Liveness
	}
	if d.Status == session.Loading || d.Status == session.Deleting {
		return cur
	}
	return session.LivenessForStatus(d.Status)
}
