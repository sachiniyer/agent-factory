package app

import (
	"encoding/json"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// prInfoStaleAfter is how long a fetched PR info entry is considered fresh.
// Selection changes within this window do not re-trigger a fetch.
const prInfoStaleAfter = 60 * time.Second

// -- Ticker message types --

type tickUpdateMetadataMessage struct{}
type tickUpdatePRInfoMessage struct{}
type tickPendingInstancesMessage struct{}
type tickRefreshExternalMessage struct{}

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

// tickRefreshExternalCmd reconciles the sidebar with on-disk state
// to pick up changes made via the CLI (e.g. `af sessions create/kill`).
// Runs every 3s.
var tickRefreshExternalCmd = func() tea.Msg {
	time.Sleep(3 * time.Second)
	return tickRefreshExternalMessage{}
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
	return func() tea.Msg {
		fetchStart := time.Now()
		detachTraceMark("fetchPRInfoCmd-goroutine-entry")
		info, err := prInfoFetcher(repoPath, branch)
		detachTrace(fetchStart, "fetchPRInfoCmd-prInfoFetcher-returned")
		return prInfoUpdatedMsg{instance: inst, branch: branch, info: info, err: err}
	}
}

// -- Sync methods --

// mergePendingInstances loads pending instances written by scheduled task runs,
// adds matching ones to the sidebar, and routes others to their per-repo storage.
func (m *home) mergePendingInstances() int {
	pendingData, err := task.LoadAndClearPendingInstances()
	if err != nil {
		log.WarningLog.Printf("failed to load pending instances: %v", err)
		return 0
	}

	sidebarTitles := m.sidebar.GetInstanceTitles()

	var otherRepoPending []session.InstanceData
	var mergedCount int
	for _, data := range pendingData {
		rid := config.RepoIDFromRoot(data.Worktree.RepoPath)
		if rid != m.repoID {
			otherRepoPending = append(otherRepoPending, data)
			continue
		}

		// If an entry with the same title already exists in the sidebar, decide
		// whether to replace it or skip the pending instance. A scheduled task
		// rerun may recreate the same tmux session name under a different
		// worktree path (worktrees gain a numeric suffix on collision), so we
		// cannot rely on TmuxAlive() alone to tell whether the sidebar
		// instance still reflects the pending one.
		//
		// We capture the collision decision BEFORE attempting FromInstanceData
		// so that we only remove the existing sidebar instance after we know
		// the replacement could be constructed. Otherwise a transient
		// FromInstanceData failure plus a subsequent successful merge in the
		// same batch would cause SaveInstances to overwrite disk without the
		// removed entry, permanently losing it (issue #367).
		shouldReplace := false
		if sidebarTitles[data.Title] {
			skip := false
			for _, existing := range m.sidebar.GetInstances() {
				if existing.Title != data.Title {
					continue
				}
				if instanceCollisionShouldSkip(existing.GetWorktreePath(), data.Worktree.WorktreePath, existing.CreatedAt, data.CreatedAt, existing.TmuxAlive(), isTransientStatus(existing.GetStatus())) {
					log.WarningLog.Printf("skipping pending instance %q: already exists and is alive", data.Title)
					skip = true
				} else {
					shouldReplace = true
				}
				break
			}
			if skip {
				continue
			}
		}

		pendingInstance, err := session.FromInstanceData(data)
		if err != nil {
			log.WarningLog.Printf("failed to restore pending instance %s: %v", data.Title, err)
			// Do NOT mutate the sidebar — the existing instance (if any)
			// must remain so SaveInstances does not drop it from disk.
			continue
		}

		if shouldReplace {
			log.InfoLog.Printf("replacing stale instance %q with new pending instance", data.Title)
			m.sidebar.RemoveInstanceByTitle(data.Title)
			delete(sidebarTitles, data.Title)
		}

		m.sidebar.AddInstance(pendingInstance)()
		pendingInstance.SetAutoYes(m.autoYes)
		sidebarTitles[data.Title] = true
		mergedCount++
	}

	if len(otherRepoPending) > 0 {
		grouped := make(map[string][]session.InstanceData)
		for _, d := range otherRepoPending {
			rid := config.RepoIDFromRoot(d.Worktree.RepoPath)
			grouped[rid] = append(grouped[rid], d)
		}
		for rid, group := range grouped {
			if err := config.UpdateRepoInstances(rid, func(existing json.RawMessage) (json.RawMessage, error) {
				var existingData []session.InstanceData
				if existing != nil && string(existing) != "[]" && string(existing) != "null" {
					if err := json.Unmarshal(existing, &existingData); err != nil {
						return nil, fmt.Errorf("failed to parse existing instances for repo %s: %w", rid, err)
					}
				}
				existingData = upsertInstanceDataByTitle(existingData, group)
				return json.Marshal(existingData)
			}); err != nil {
				log.WarningLog.Printf("failed to merge pending instances for repo %s: %v", rid, err)
			}
		}
	}

	if mergedCount > 0 {
		if err := m.storage.SaveInstances(m.sidebar.GetInstances()); err != nil {
			log.WarningLog.Printf("failed to save merged instances: %v", err)
		}
	}

	return mergedCount
}

// refreshExternalInstances reconciles the sidebar's in-memory instances with
// the on-disk instances.json. Returns true if anything changed.
func (m *home) refreshExternalInstances() bool {
	diskData, err := m.storage.LoadInstanceData()
	if err != nil {
		log.WarningLog.Printf("failed to load instance data for refresh: %v", err)
		return false
	}

	sidebarTitles := m.sidebar.GetInstanceTitles()
	diskTitles := make(map[string]bool, len(diskData))
	for _, d := range diskData {
		diskTitles[d.Title] = true
	}

	changed := false

	// Add instances that exist on disk, and replace stale sidebar instances
	// whose title was reused by a CLI kill+recreate.
	//
	// A title present in both the sidebar and on disk usually means the
	// instance is unchanged, so we skip it. But when a session is killed and
	// recreated under the same title via the CLI, the dead in-memory instance
	// shadows the freshly created on-disk one: title-only membership makes the
	// add pass skip it and the remove pass keep the corpse, leaving the new
	// session invisible (#765). Reuse the same liveness/staleness check as
	// mergePendingInstances — when the colliding sidebar instance is stale
	// (different worktree, its tmux session is gone, or it was superseded by a
	// more recently created on-disk record) swap it for the disk instance.
	// Construct the replacement BEFORE removing the existing entry so a
	// transient FromInstanceData failure can't drop it from disk on save.
	for _, d := range diskData {
		shouldReplace := false
		if sidebarTitles[d.Title] {
			skip := true
			for _, existing := range m.sidebar.GetInstances() {
				if existing.Title != d.Title {
					continue
				}
				if !instanceCollisionShouldSkip(existing.GetWorktreePath(), d.Worktree.WorktreePath, existing.CreatedAt, d.CreatedAt, existing.TmuxAlive(), isTransientStatus(existing.GetStatus())) {
					skip = false
					shouldReplace = true
				}
				break
			}
			if skip {
				continue
			}
		}

		inst, err := session.FromInstanceData(d)
		if err != nil {
			log.WarningLog.Printf("failed to restore external instance %q: %v", d.Title, err)
			// Leave any colliding sidebar instance untouched so SaveInstances
			// does not drop it from disk.
			continue
		}

		if shouldReplace {
			log.InfoLog.Printf("swapping stale sidebar instance %q for recreated on-disk instance", d.Title)
			m.sidebar.RemoveInstanceByTitle(d.Title)
			delete(sidebarTitles, d.Title)
		}

		m.sidebar.AddInstance(inst)()
		inst.SetAutoYes(m.autoYes)
		sidebarTitles[d.Title] = true
		changed = true
	}

	// Remove instances that exist in sidebar but not on disk.
	// Skip instances with Loading status (TUI is currently creating them).
	// Collect removals first to avoid modifying the slice during iteration.
	var toRemove []*session.Instance
	for _, inst := range m.sidebar.GetInstances() {
		if !diskTitles[inst.Title] && inst.GetStatus() != session.Loading {
			toRemove = append(toRemove, inst)
		}
	}
	for _, inst := range toRemove {
		m.sidebar.RemoveInstanceByTitle(inst.Title)
		changed = true
	}

	return changed
}

// instanceCollisionShouldSkip decides whether to keep an existing sidebar
// instance when an incoming on-disk or pending instance reuses its title. It
// returns true when the incoming instance should be skipped (the sidebar
// instance is still the authoritative live session), false when the sidebar
// instance is stale and must be replaced.
//
// A Loading sidebar instance is never replaced (#808): it is the placeholder
// for an in-flight TUI creation of this very session — the daemon persists
// the record to instances.json before the start RPC returns, so the on-disk
// row appearing while the placeholder is still Loading is the normal
// mid-create state, not a stale corpse. Its CreatedAt also predates the
// daemon-side record, so the #765 newer-CreatedAt rule below would otherwise
// always swap it, orphaning the pointer the instanceStartedMsg handler later
// passes to ReplaceInstance and leaving two same-title sidebar rows.
//
// A Deleting sidebar instance is likewise never replaced (#844): its on-disk
// record legitimately still exists until the background teardown finishes,
// and swapping the row for a disk-built copy would erase the Deleting marker —
// resurrecting a kill-enabled row for a session that is mid-teardown. The
// transient flag below covers both states.
//
// The incoming instance supersedes the existing one when:
//   - both worktree paths are known and differ — a scheduled task rerun
//     creates a new worktree with a numeric suffix while reusing the tmux
//     session name, so TmuxAlive() would wrongly report the sidebar instance
//     as live (issue #255); or
//   - the incoming record was created more recently — a CLI kill+recreate
//     reuses the same title, and because both the worktree path and the tmux
//     session name are derived deterministically from the title, the recreated
//     session collides with the corpse on both. Neither worktree nor
//     TmuxAlive() can then distinguish them; the newer CreatedAt can (#765).
//
// Otherwise, fall back to the tmuxAlive signal.
func instanceCollisionShouldSkip(existingWorktreePath, incomingWorktreePath string, existingCreatedAt, incomingCreatedAt time.Time, tmuxAlive, existingTransient bool) bool {
	if existingTransient {
		return true
	}
	if existingWorktreePath != "" && incomingWorktreePath != "" && existingWorktreePath != incomingWorktreePath {
		return false
	}
	if incomingCreatedAt.After(existingCreatedAt) {
		return false
	}
	return tmuxAlive
}

// isTransientStatus reports whether an in-memory sidebar instance is in a
// state owned by an in-flight TUI operation — Loading (creation, #808) or
// Deleting (async kill, #844) — during which background syncs must neither
// replace nor reap it.
func isTransientStatus(status session.Status) bool {
	return status == session.Loading || status == session.Deleting
}

func upsertInstanceDataByTitle(existing, incoming []session.InstanceData) []session.InstanceData {
	index := make(map[string]int, len(existing))
	for i := range existing {
		index[existing[i].Title] = i
	}
	for _, data := range incoming {
		if i, ok := index[data.Title]; ok {
			existing[i] = data
			continue
		}
		index[data.Title] = len(existing)
		existing = append(existing, data)
	}
	return existing
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
