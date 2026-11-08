package app

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
)

// -- Ticker message types --

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
	// remote is the target's registry and tasks read on this poll, nil locally.
	remote    *remoteReads
	remoteSeq uint64
	// alarms carries any persistent watch-task delivery-failure alarms projected
	// on the same daemon snapshot (#1238). It rides the session snapshot because
	// it is a field of the one authoritative Snapshot response, not a separate
	// subsystem; handleSnapshot mirrors it into the alarm banner state.
	alarms []daemon.DeliveryAlarm
	// skippedRepos carries the daemon's startup-skipped repo set for this scope
	// (#4742): repos whose instances.json was corrupted, unreadable, written by a
	// newer schema, or had rows that fail to load, so the daemon dropped them at
	// startup and the snapshot's Instances list is incomplete. handleSnapshot
	// raises a standing notice when the set names m.repoID so the TUI never
	// presents the partial list as complete — the same incompleteness channel the
	// CLI's list/get/whoami refuse on and the web shows a persistent notice for
	// (#5413). Routed the same way DeliveryAlarms travels: a field of the one
	// authoritative Snapshot response, mirrored into operator-facing state.
	skippedRepos []daemon.SkippedRepo
	// allRepos is the daemon's cross-repo session list (RepoID:"") fetched on the
	// same poll, off-loop, so the always-visible bottom Projects section can
	// refresh its per-repo counts live when sessions change in ANOTHER repo
	// (#1590). allReposErr is carried separately (like tasksErr) so a failed
	// cross-repo read leaves the last-known project rows intact rather than
	// blanking the section on a hiccup.
	allRepos    []session.InstanceData
	allReposErr error
}

// -- Ticker commands --
// Each ticker sleeps for a fixed interval, then returns its message type to
// re-enter Update(). The ticker is re-scheduled at the end of its handler.

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
	lifetime := m.ctx
	remoteSeq := m.remote.seq // the task-mutation count this poll starts from
	allRepos := allReposSnapshotFetcher
	liveArming := liveTaskArmingFetcher
	return func() tea.Msg {
		resp, err := fetch(repoID)
		// Re-read the ACTIVE repo's tasks on the same poll so an out-of-band task
		// change live-projects into the TUI (#1168). Scoped to the captured
		// repoRoot, not cwd, so a poll after an in-place project switch (#1461)
		// reads the switched-to project's tasks — not the launch repo's. Its own
		// error is carried separately so a warming-daemon RPC failure never
		// suppresses a successful disk task read.
		var remote *remoteReads
		var tasks []task.Task
		var tasksErr error
		if isRemoteTarget() {
			remote = fetchRemoteReads(lifetime)
			if repoID != "" {
				tasks, tasksErr = filterTasksToProject(remote.tasks, repoRoot, repoID), remote.tasksErr
			}
		} else {
			tasks, tasksErr = task.LoadTasksForKnownRepo(repoRoot, repoID)
		}
		// The disk read is the DEFINITION and stays authoritative; what it cannot
		// see is whether the running scheduler is holding each task, which is
		// observable only inside the daemon. Fetched here, beside the snapshot,
		// rather than by teaching the rail to talk to the daemon on its own: one
		// poll, one place, and the rail keeps its single input (#3626).
		//
		// On a failed fetch this is a no-op and every record keeps arming UNKNOWN,
		// which is what the rail renders as the labelled cron-expression fallback.
		// Nothing here can turn a missed poll into a not-armed accusation.
		observed, armingErr := liveArming()
		if armingErr == nil {
			tasks = task.ApplyLiveArming(tasks, observed)
		}
		// Fetch the cross-repo session list on the same off-loop poll so the
		// always-visible Projects section's per-repo counts stay live (#1590).
		// Read here (not in the handler) to keep the daemon RPC off the event
		// loop; its error is carried separately so a hiccup keeps the last rows.
		// The cross-repo skip set is consumed by the project picker's own fetch
		// (buildProjectListWithSnapshotStatus), not this poll, so it is discarded
		// here — the repo-scoped skip set (resp.SkippedRepos) is what the
		// handleSnapshot notice reads.
		allReposData, _, allReposErr := allRepos(context.Background())
		return snapshotFetchedMsg{
			data: resp.Instances, alarms: resp.DeliveryAlarms, err: err,
			skippedRepos: resp.SkippedRepos,
			tasks:        tasks, tasksErr: tasksErr, repoID: repoID, remote: remote, remoteSeq: remoteSeq,
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
// (non-warming) daemon failure. Startup renders recovery and keeps normal
// snapshot polling alive; unavailable data is never presented as an empty list.
// When the daemon skipped this repo at startup (#4742), a standing notice is
// raised so the operator can tell "no sessions" from "sessions are hidden" —
// the same incompleteness channel the CLI and web surface.
func (m *home) coldStartFromSnapshot() error {
	data, skipped, err := m.fetchColdStartSnapshot(m.repoID)
	m.snapshotUnavailable = err != nil
	if err != nil {
		return err
	}
	m.materializeSnapshot(data)
	m.applySkippedRepo(skippedRepoForRepoID(skipped, m.repoID))
	return nil
}

// defaultSnapshotBuildWorkers bounds how many snapshot records are restored at once.
// Each restore spawns a few tmux subprocesses (has-session, list-panes,
// display-message), so serial builds made the launch — and a project switch —
// cost grow with the number of sessions in the project (#6423); an unbounded
// fan-out would instead flood tmux with a subprocess per session.
const defaultSnapshotBuildWorkers = 16

// materializeSnapshot builds each snapshot record into a live instance and adds
// it to the projection. Split out of coldStartFromSnapshot so switchProject can
// fetch a target project's snapshot up front — while the outgoing project is
// still intact — and only materialize it once the fetch has succeeded (#1788).
// A single unrestorable record is logged and skipped, never aborting the batch.
//
// The restores overlap (defaultSnapshotBuildWorkers at a time) but the store is fed on
// this goroutine in snapshot order, so the sidebar order, the adopted-op
// bookkeeping and the store's own single-writer contract are unchanged.
func (m *home) materializeSnapshot(data []session.InstanceData) {
	workers := m.snapshotBuildWorkers
	if workers <= 0 {
		workers = defaultSnapshotBuildWorkers
	}
	if m.primeSessionSnapshot != nil {
		// One `tmux list-panes -a` answers the existence, generation and pane-row
		// probes of every restore in the batch (#6423); released when it is done.
		defer m.primeSessionSnapshot()()
	}
	for _, inst := range buildSnapshotInstances(data, workers) {
		// A row BUILT from a snapshot can already carry a daemon-owned op, and it is
		// just as adopted as one mirrored onto an existing row — the cold-start
		// variant of the same corpse (#3005).
		m.adoptedOpsFor().note(inst, inst.GetInFlightOp())
		m.store.AddInstance(inst)()
	}
}

// primeTmuxSessionSnapshot lists every tmux pane once and primes the restore-time
// probes with it. A failed listing (no tmux server, a remote target) primes
// nothing, and every restore then asks tmux per session exactly as before.
func primeTmuxSessionSnapshot() func() {
	snap, err := tmux.CaptureSessionSnapshot()
	if err != nil {
		return func() {}
	}
	return tmux.PrimeSessionSnapshot(snap)
}

// buildSnapshotInstances restores the snapshot records with at most `workers`
// builds in flight and returns the successes in snapshot order. Records still
// awaiting their worktree are skipped, and a record that fails to build is logged
// and dropped: the session's tmux/worktree may have been destroyed externally,
// and that must not fail the whole launch.
func buildSnapshotInstances(data []session.InstanceData, workers int) []*session.Instance {
	build := buildInstanceFromSnapshot // read once: a test seam, swapped on another goroutine
	// One context for the whole batch: every restore of a project shares a single
	// repo/config resolution instead of repeating it per session (#6446).
	rc := session.NewRestoreContext()
	built := make([]*session.Instance, len(data))
	jobs := make(chan int)
	var wg sync.WaitGroup
	if workers > len(data) {
		workers = len(data)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				inst, err := build(data[i], rc)
				if err != nil {
					log.WarningLog.Printf("skipping session %q from snapshot: %v", data[i].Title, err)
					continue
				}
				built[i] = inst
			}
		}()
	}
	for i, d := range data {
		if snapshotAwaitingWorktree(d) {
			continue
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	out := built[:0]
	for _, inst := range built {
		if inst != nil {
			out = append(out, inst)
		}
	}
	return out
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
// Returns the skip set alongside the instances so the caller can raise the
// skipped-repo notice (#4742) — a repo the daemon dropped at startup cold-starts
// as a clean empty sidebar without it.
func (m *home) fetchColdStartSnapshot(repoID string) ([]session.InstanceData, []daemon.SkippedRepo, error) {
	deadline := time.Now().Add(coldStartWarmupWait)
	announced := false
	for {
		resp, err := m.snapshotFetcher(repoID)
		if err == nil {
			return resp.Instances, resp.SkippedRepos, nil
		}
		if !daemon.IsDaemonStartingErr(err) {
			return nil, nil, err
		}
		if !announced {
			// Pre-TUI stdout, matching newHome's other startup messages: tell the
			// user we are waiting on the daemon rather than showing an empty list.
			fmt.Println("Restoring sessions… (daemon warming up)")
			announced = true
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("daemon is still restoring sessions after %s; try again in a moment", coldStartWarmupWait)
		}
		time.Sleep(coldStartWarmupPoll)
	}
}

// -- Sync methods --

// snapshotFailureGrace keeps transient poll failures from replacing a loaded layout.
const snapshotFailureGrace = 3 * time.Second

// handleSnapshot applies the daemon's authoritative snapshot and reports whether
// the caller needs to repaint. Failed polls retain the last-known projection;
// non-starting failures show recovery only after the grace window. A warming
// daemon (#829) is retried without changing the view. Success clears the outage
// and its timer. Cold-start recovery remains immediate because no data was loaded.
// The Snapshot RPC is the sole sync path (#960 PR 4); there is no disk fallback.
func (m *home) handleSnapshot(msg snapshotFetchedMsg) bool {
	wasUnavailable := m.snapshotUnavailable
	if msg.err != nil {
		if daemon.IsDaemonStartingErr(msg.err) {
			// Daemon still restoring (#829). Retain the current view and
			// failure window; only a successful snapshot ends the outage.
			return false
		}
		now := time.Now
		if m.snapshotClock != nil {
			now = m.snapshotClock
		}
		at := now()
		if m.snapshotFailureSince == nil {
			m.snapshotFailureSince = &at
		}
		if at.Sub(*m.snapshotFailureSince) >= snapshotFailureGrace {
			m.snapshotUnavailable = true
		}
		log.WarningLog.Printf("failed to fetch daemon snapshot: %v", msg.err)
		return m.snapshotUnavailable != wasUnavailable
	}
	m.snapshotUnavailable = false
	m.snapshotFailureSince = nil
	changed := wasUnavailable
	// An empty active scope owns no sessions, so there is nothing to reconcile
	// (#2864). The daemon reads an empty repoID as "every repo" — the same
	// all-repos answer the cold start deliberately refuses in registry mode
	// (#2477) — and those rows are not merely irrelevant, they are INERT: every
	// per-session verb keys on m.repoID, and resolveSessionActionTarget requires
	// target.repoID == m.repoID, so each one silently no-ops. Reconciling them
	// filled the rail with sessions whose `s` and `enter` do nothing, undoing
	// newHome's refusal one poll later.
	//
	// The alarms below are deliberately NOT gated. deliveryAlarms reads an empty
	// repoID as every repo too, but an alarm is a notification, not a target: it
	// needs no active scope to be worth showing, and suppressing it would hide a
	// real delivery failure from the one mode a user is most likely to be idling
	// in. The Projects section keeps its own counts from msg.allRepos, which this
	// does not touch, so the cross-repo view a registry-mode user actually needs
	// stays live.
	if m.repoID != "" {
		changed = m.reconcileSnapshot(msg.data) || changed
	}
	// Surface the skipped-repo incompleteness channel (#4742): when the daemon
	// dropped this repo at startup, the snapshot's Instances list is truncated and
	// must not be presented as complete. applySkippedRepo raises a standing notice
	// (persistent one-line condition on the status bar) so the operator can tell
	// "no sessions" from "sessions are hidden", and queues a transient notice with
	// the full repair hint so `E details` can open it. Routed the same way
	// applyDeliveryAlarms travels — a field of the one authoritative Snapshot
	// response, mirrored into operator-facing state. Not gated on m.repoID: a
	// skipped repo in the cross-repo snapshot still hides sessions the Projects
	// section counts, and the notice names the repo so the operator knows which
	// project is affected.
	if m.applySkippedRepo(skippedRepoForRepoID(msg.skippedRepos, m.repoID)) {
		changed = true
	}
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
			TaskName:       a.TaskName,
			Target:         a.TargetSession,
			Pending:        a.Pending,
			PendingUnknown: a.PendingUnknown,
			Since:          a.Since,
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

// applySkippedRepo mirrors the snapshot's skipped-repo incompleteness channel
// (#4742) into the TUI's operator-facing notice state. When the current repo is
// in the daemon's startup skip set, a standing notice is raised on the status bar
// so the operator can tell "no sessions" from "sessions are hidden" — the same
// incompleteness the CLI's list/get/whoami refuse on and the web shows a
// persistent notice for (#5413). When the repo is no longer skipped (the daemon
// re-read the file on a polling refresh, #603), the standing notice is retracted.
//
// A transition into a skipped state — or a reason/FailedRows/RepoID change —
// queues a pending transient notice (pendingSkippedRepoNotice) carrying the full
// reason-specific repair hint, which the caller drains as a tea.Cmd so it lands
// on the bar. The standing notice takes over after the transient fades. A
// same-entry poll does not re-fire, so the 750ms cadence never churns the bar.
//
// The skip's full repair hint is bound to the skip condition itself
// (skippedRepoRetained), not the shared most-recent-notice slot any later
// transient overwrites, so `E details` opens it for as long as the skip is true
// (#4742 review). The standing notice is composed with the web-listener's
// (applyStandingNotice) so the two conditions never overwrite each other on
// every poll (#4742 review).
//
// Returns whether the visible state changed (the caller repaints on a diff).
// Only ever called on a successful snapshot: a transient RPC error leaves the
// last-known notice up rather than clearing a real skip on a hiccup, matching
// applyDeliveryAlarms.
func (m *home) applySkippedRepo(cur *daemon.SkippedRepo) bool {
	if !skippedRepoNoticeChanged(m.skippedRepo, cur) {
		return false
	}
	m.skippedRepo = cur
	if cur == nil {
		// The repo was skipped and is now repaired (the daemon re-read its
		// instances.json on a polling refresh). Retract the skip's condition-
		// owned details and re-composing the standing bar (the web-listener
		// notice, if any, reappears) so the bar stops naming a condition that
		// is no longer true.
		m.skippedRepoRetained = ""
		m.applyStandingNotice()
		return true
	}
	// Raise the standing notice for the persistent condition (composed with
	// the web-listener notice) and bind the full repair hint to the skip
	// condition so `E details` opens it independently of the shared retained
	// slot. A transient copy is queued for the bar so the operator sees the
	// full hint once before the standing line takes over.
	m.applyStandingNotice()
	m.skippedRepoRetained = skippedRepoNotice(cur).Error()
	m.pendingSkippedRepoNotice = m.skippedRepoRetained
	return true
}

// applyStandingNotice composes the skip and web-listener standing conditions
// onto the bar's single standing slot. The two conditions are tracked in
// separate home fields so a 15-second listener poll cannot overwrite the
// skip's hidden-sessions warning and a skip retraction cannot erase a still-
// valid listener warning; both are re-applied here whenever either changes
// (#4742 review). When both are up they are joined with " · " — the bar's own
// separator for multi-line notices — so a watcher can still tell which
// condition(s) hold, truncated by the standing line's width math.
func (m *home) applyStandingNotice() {
	skip := skippedRepoStandingNotice(m.skippedRepo)
	listener := m.webListenerStanding
	switch {
	case skip != "" && listener != "":
		m.errBox.SetStanding(skip + " · " + listener)
	case skip != "":
		m.errBox.SetStanding(skip)
	case listener != "":
		m.errBox.SetStanding(listener)
	default:
		m.errBox.SetStanding("")
	}
}

// refreshTasks mirrors an out-of-band tasks.json change (a CLI/daemon `af tasks
// add|update|remove`, or a run that bumped a task's last-run status) into the
// running TUI, closing the live-projection gap that left a CLI-created task
// invisible until relaunch (#1168 — the tasks sibling of the #959 tab fix).
// Tasks are a disk-backed store shared between the TUI and the daemon, so the
// TUI re-reads them on the snapshot poll rather than through the daemon's
// session Snapshot. The automations rail (store) and the tasks overlay pane both
// mirror the fresh list; the pane reconciles it around whatever the user is
// holding — an open form, unsaved or failed edits, queued deletions — so a
// background refresh never clobbers them and never leaves the rest stale
// (#4487). Returns whether anything visible changed (the caller repaints on a
// diff). A read error leaves the last-known list intact, matching handleSnapshot.
func (m *home) refreshTasks(tasks []task.Task, tasksErr error) bool {
	changed := m.automations.TaskPane().SetUnavailable(tasksErr)
	if tasksErr != nil {
		log.WarningLog.Printf("failed to refresh tasks: %v", tasksErr)
		return changed
	}
	if !reflect.DeepEqual(m.store.GetTasks(), tasks) {
		countChanged := m.store.NumTasks() != len(tasks)
		m.store.SetTasks(tasks)
		changed = true
		if countChanged {
			// The task count feeds the rail's automations-section height
			// (#1126), read only in relayout — reflow on this poll, as the
			// TUI's own write paths do, so a CLI-added first task appears and
			// a removed last one goes (#4965). A same-count edit leaves every
			// grid input alone, so the 750ms poll doesn't churn the layout.
			m.relayout()
		}
	}
	// A held draft keeps the pane's list different from disk, so the pane, not
	// this comparison, says whether the reconcile changed anything visible.
	sp := m.automations.TaskPane()
	if !reflect.DeepEqual(sp.GetTasks(), tasks) && sp.SetTasks(tasks) {
		changed = true
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
//     its tab list mutated — which is the #959 "live display" fix
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
			//
			// Only a LOCAL op vetoes. An op adopted from a daemon snapshot has no
			// completion handler waiting on it, so there is no handshake to orphan —
			// and its release only ever arrives as a later OpNone snapshot for THIS
			// identity, which by definition never comes once the title has been
			// recreated. Vetoing on it left the corpse on the rail forever (#3005).
			if m.adoptedSnapshotOps.vetoesReconcile(inst) {
				continue
			}
			// Only on SUCCESS: swapInstanceFromSnapshot returns false and leaves the
			// old row in place when the replacement cannot be built, and forgetting
			// first would reclassify its adopted op as local — vetoing every later
			// retry and turning a transient build failure into the permanent stale row
			// this change exists to prevent.
			if m.swapInstanceFromSnapshot(d, selectedTitle) {
				m.adoptedSnapshotOps.forget(inst)
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
			if m.swapInstanceFromSnapshot(d, selectedTitle) {
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
		// Same distinction as the identity swap above: a local op owns its own
		// removal through its completion handler, an adopted one does not, and the
		// snapshot is authoritative about whether the session still exists (#3005).
		if m.adoptedSnapshotOps.vetoesReconcile(inst) {
			continue
		}
		toRemove = append(toRemove, inst)
	}
	for _, inst := range toRemove {
		m.store.RemoveInstanceByTitle(inst.Title)
		// The map is keyed by pointer, so an entry for a row that has left the store
		// would never be read again but would keep the instance alive.
		m.adoptedSnapshotOps.forget(inst)
		changed = true
	}
	// Provenance is keyed by pointer, so an entry for a row that has left the store
	// by ANY route — removal, swap, project switch — would leak and keep that
	// instance reachable. Reconciling the map against the live set here covers every
	// exit without each one having to remember this map (#3005).
	m.adoptedSnapshotOps.pruneTo(m.store.GetInstances())

	// Re-pin the selection to the same logical instance (by title) if it
	// survived the reconcile, BEFORE the pane-prune relayout. Re-resolving by
	// title correctly re-pins across a swap because the rebuilt instance keeps
	// the same title (#969). The store's SelectInstance records the assertion;
	// the sidebar moves its cursor onto the asserted row on its next read. If
	// the selected title is gone from the snapshot, leave the sidebar's own
	// clamp behavior as-is.
	//
	// The re-pin runs ahead of pruneDeadPanes's relayout for the same #5298
	// reason the swap path pre-pins ahead of its own: that relayout reads the
	// sidebar (the footer's row-verb target), and a rebuild running while the
	// cursor still holds its removal-drifted flat index commits the drifted
	// row via pushSelection and rewrites the lastCursor* identity evidence the
	// re-pin's replaced-session (by-name) tab lookup depends on. Running the
	// assertion first turns that relayout's sync into a cursor move on intact
	// evidence instead of a drift commit. The re-pin still runs after the
	// per-row loop and the removals above (which shift indices), so it
	// re-resolves the survivor by title against the post-removal roster.
	if selectedTitle != "" {
		if inst := m.store.GetInstanceByTitle(selectedTitle); inst != nil {
			m.store.SelectInstance(inst)
		}
	}

	// Prune panes whose backing session can no longer render — a removed
	// instance takes its open panes with it (#1088), and a session archived out
	// of band (`af sessions archive` while the TUI runs, #1028) leaves its pane
	// dangling on a torn-down session. Do it here, not just on the next
	// selectionChanged tick, so the focus ring and pane layout are consistent
	// the moment the reconcile returns. Unconditional: pruneDeadPanes is O(open
	// panes) and only relayouts when it actually closed something, so a reconcile
	// with nothing to prune is a cheap no-op. The selection re-pin above already
	// ran, so this relayout's sidebar sync commits a cursor move, not a drift.
	if m.pruneDeadPanes() {
		m.relayout()
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

// snapshotAwaitingWorktree reports whether a record is the daemon's pending
// create projection: published (so clients see the session at once) before the
// worktree exists, so its empty worktree path is an expected transient and the
// next reconcile picks the row up. The same empty path on a record with no
// create in flight stays a real fault and keeps its warning, whatever its
// liveness says (the pending projection itself claims LiveReady).
func snapshotAwaitingWorktree(d session.InstanceData) bool {
	return d.InFlightOp == session.OpCreating && d.Worktree.WorktreePath == ""
}

// addInstanceFromSnapshot builds a live instance from a snapshot record and adds
// it to the projection store. Returns true on success (a real change). A build
// failure is logged and skipped — a single unrestorable record must not abort
// the whole reconcile.
func (m *home) addInstanceFromSnapshot(d session.InstanceData) bool {
	if snapshotAwaitingWorktree(d) {
		log.InfoLog.Printf("deferring instance %q: create in flight, worktree not populated yet", d.Title)
		return false
	}
	inst, err := buildInstanceFromSnapshot(d, nil)
	if err != nil {
		log.WarningLog.Printf("failed to build instance %q from snapshot: %v", d.Title, err)
		return false
	}
	m.adoptedOpsFor().note(inst, inst.GetInFlightOp())
	m.store.AddInstance(inst)()
	return true
}

// swapInstanceFromSnapshot replaces a stale same-title row with a freshly
// built instance for the recreated session (#765), preserving the selection.
// Built BEFORE the swap so a transient build failure leaves the existing row in
// place rather than dropping it. selectedTitle is the title of the row the
// sidebar cursor rested on at the start of this reconcile ("" while it rested on
// a section header), captured once before the per-row loop — see reconcileSnapshot.
func (m *home) swapInstanceFromSnapshot(d session.InstanceData, selectedTitle string) bool {
	if snapshotAwaitingWorktree(d) {
		return false
	}
	inst, err := buildInstanceFromSnapshot(d, nil)
	if err != nil {
		log.WarningLog.Printf("failed to build recreated instance %q from snapshot: %v", d.Title, err)
		return false
	}
	// The replacement can arrive already carrying a daemon-owned op, exactly like
	// the other two snapshot builders. Recorded here so the NEW pointer starts with
	// provenance — without it the successor inherits the original defect the moment
	// its own identity changes (#3005).
	m.adoptedOpsFor().note(inst, inst.GetInFlightOp())
	// ReplaceInstance re-points any open panes at the rebuilt instance, but
	// the recreated session's tab SET may differ from the corpse's — capture
	// the outgoing slot→identity list so the shared pane reconcile can close
	// panes whose tab didn't come back and re-bind ones whose slot moved (#1088).
	//
	// This is the REPLACED-session domain: the swap mints an entirely new session,
	// so every tab id differs from the corpse's by construction. Matching on the
	// id here would read every tab as missing and close every pane the swap was
	// supposed to preserve, so the panes follow the equivalent slot by NAME.
	var oldKeys []tabSlotKey
	if old := m.store.GetInstanceByTitle(d.Title); old != nil {
		oldKeys = paneTabKeys(old)
	}
	if !m.store.ReplaceInstanceByTitle(d.Title, inst) {
		// The row vanished between read and swap; add it fresh.
		m.store.AddInstance(inst)()
	}
	// Record the roster the TUI observed at swap time: the daemon builds a
	// recreated session's tabs one call at a time, so a poll can swap in a
	// partial roster, and the real-TUI scenarios (scripts/tui-swap-pane-repin-
	// scenario.sh, #5620) need this evidence to tell that setup race from a
	// re-pin defect.
	tabNames := make([]string, 0, len(inst.GetTabs()))
	for _, tab := range inst.GetTabs() {
		tabNames = append(tabNames, tab.Name)
	}
	log.InfoLog.Printf("same-title swap of %q: replacement roster [%s]", d.Title, strings.Join(tabNames, ","))
	// Pre-pin the cursor to the surviving selectedTitle BEFORE any
	// sidebar-reading relayout this swap can trigger (#5298 ordering, applied
	// to the swap path). ReplaceInstanceByTitle re-sorts the whole projection,
	// so a non-selected swap drifts the cursor's flat index too — an open pane
	// on the swapped session makes reconcilePanesForTabs call relayout, and a
	// cursor still holding its removal/re-sort drifted flat index then commits
	// a different row through pushSelection, rewriting lastCursor* and
	// clobbering that session's tab identity ahead of the tail by-title re-pin.
	// Re-pin the surviving selectedTitle (by title) so the relayout's sidebar
	// sync is a cursor move on intact lastCursor* evidence instead of a drift
	// commit: a non-selected swap re-resolves the still-present row the cursor
	// was on, and a selected-swap resolves to the fresh replacement
	// ReplaceInstanceByTitle kept under that title (same instance the old
	// selectedTitle==d.Title path pinned). ReplaceInstance's silent re-point
	// deliberately never asserts — a cursor resting on a section header must
	// not be yanked onto any row — so the empty-title header guard is retained
	// here, the same capture reconcileSnapshot uses to drive the tail re-pin.
	// The sidebar consumes the assertion lazily on its next read; the pane
	// reconcile's relayout (when a pane closed or re-bound) is that read, so
	// moveCursorToInstance resolves the replacement's equivalent tab by name
	// from the corpse's intact evidence instead of letting pushSelection commit
	// the stale flat index and rewrite lastCursor* first.
	if selectedTitle != "" {
		if sel := m.store.GetInstanceByTitle(selectedTitle); sel != nil {
			m.store.SelectInstance(sel)
		}
	}
	// The tree selection is another binding into the same replaced-session
	// roster. Carry it through the same name domain as panes: every ID is freshly
	// minted, while the equivalent replacement tab keeps its name (#1991). Bound
	// before the pane reconcile for the same #5298 reason as the in-place path —
	// a sidebar-reading relayout must not run while the selection binding is
	// unresolved. The pre-pin above closed the swap's window for the CURSOR too
	// (the in-place path uses SyncCursorToActiveTab; the swap re-sorts, so it
	// re-pins by identity via SelectInstance instead), leaving only the active-tab
	// binding for remapActiveTabForTabs to resolve.
	m.remapActiveTabForTabs(inst, oldKeys, replacedSessionTabs)
	if m.reconcilePanesForTabs(inst, oldKeys, replacedSessionTabs) {
		m.relayout()
	}
	return true
}
