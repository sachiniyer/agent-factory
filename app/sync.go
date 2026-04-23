package app

import (
	"encoding/json"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
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
type prInfoUpdatedMsg struct {
	instance *session.Instance
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
// fresh). Using force=true ignores the freshness check — for tick-driven
// refreshes of the selected instance.
func fetchPRInfoCmd(inst *session.Instance, force bool) tea.Cmd {
	if inst == nil || inst.IsRemote() {
		return nil
	}
	if !force && inst.PRInfoAge() < prInfoStaleAfter {
		return nil
	}
	repoPath, branch := inst.FetchPRInfoSnapshot()
	if repoPath == "" {
		return nil
	}
	return func() tea.Msg {
		info, err := prInfoFetcher(repoPath, branch)
		return prInfoUpdatedMsg{instance: inst, info: info, err: err}
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
		if sidebarTitles[data.Title] {
			skip := false
			for _, existing := range m.sidebar.GetInstances() {
				if existing.Title != data.Title {
					continue
				}
				if pendingInstanceCollisionShouldSkip(existing.GetWorktreePath(), data.Worktree.WorktreePath, existing.TmuxAlive()) {
					log.WarningLog.Printf("skipping pending instance %q: already exists and is alive", data.Title)
					skip = true
				} else {
					log.InfoLog.Printf("replacing stale instance %q with new pending instance", data.Title)
					m.sidebar.RemoveInstanceByTitle(data.Title)
					delete(sidebarTitles, data.Title)
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
			continue
		}
		m.sidebar.AddInstance(pendingInstance)()
		if m.autoYes {
			pendingInstance.AutoYes = true
		}
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
			existing, err := config.LoadRepoInstances(rid)
			if err != nil {
				log.WarningLog.Printf("failed to load existing instances for repo %s: %v", rid, err)
			}
			var existingData []session.InstanceData
			if existing != nil && string(existing) != "[]" && string(existing) != "null" {
				if err := json.Unmarshal(existing, &existingData); err != nil {
					log.WarningLog.Printf("failed to parse existing instances for repo %s: %v", rid, err)
				}
			}
			existingData = append(existingData, group...)
			jsonData, err := json.Marshal(existingData)
			if err != nil {
				log.WarningLog.Printf("failed to marshal instances for repo %s: %v", rid, err)
				continue
			}
			if err := config.SaveRepoInstances(rid, jsonData); err != nil {
				log.WarningLog.Printf("failed to save instances for repo %s: %v", rid, err)
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

	// Add instances that exist on disk but not in sidebar.
	for _, d := range diskData {
		if !sidebarTitles[d.Title] {
			inst, err := session.FromInstanceData(d)
			if err != nil {
				log.WarningLog.Printf("failed to restore external instance %q: %v", d.Title, err)
				continue
			}
			m.sidebar.AddInstance(inst)()
			if m.autoYes {
				inst.AutoYes = true
			}
			changed = true
		}
	}

	// Remove instances that exist in sidebar but not on disk.
	// Skip instances with Loading status (TUI is currently creating them).
	// Collect removals first to avoid modifying the slice during iteration.
	var toRemove []*session.Instance
	for _, inst := range m.sidebar.GetInstances() {
		if !diskTitles[inst.Title] && inst.Status != session.Loading {
			toRemove = append(toRemove, inst)
		}
	}
	for _, inst := range toRemove {
		m.sidebar.RemoveInstanceByTitle(inst.Title)
		changed = true
	}

	return changed
}

// pendingInstanceCollisionShouldSkip decides whether to skip a pending
// instance when an instance with the same title already exists in the
// sidebar. It returns true when the pending instance should be skipped
// (sidebar instance is still valid), false when the sidebar instance is
// stale and should be replaced.
//
// If both worktree paths are known and differ, the sidebar instance is
// stale regardless of TmuxAlive() — a scheduled task rerun creates a new
// worktree with a numeric suffix and a tmux session with the same name,
// so TmuxAlive() would incorrectly report the sidebar instance as live.
// Otherwise, fall back to the tmuxAlive signal.
func pendingInstanceCollisionShouldSkip(existingWorktreePath, pendingWorktreePath string, tmuxAlive bool) bool {
	if existingWorktreePath != "" && pendingWorktreePath != "" && existingWorktreePath != pendingWorktreePath {
		return false
	}
	return tmuxAlive
}
