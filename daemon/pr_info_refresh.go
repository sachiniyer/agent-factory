package daemon

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

const (
	prInfoRefreshInterval = 60 * time.Second
	prInfoRefreshWorkers  = 4
)

type prInfoRefreshTarget struct {
	instance *session.Instance
	id       string
	title    string
	repoID   string
	repoPath string
	branch   string
}

// prInfoRefreshTargets snapshots every stale local-worktree session eligible
// for GitHub PR discovery. Marking each target at admission prevents a second
// loop pass from starting the same lookup while the first one is in flight.
func (m *Manager) prInfoRefreshTargets() []prInfoRefreshTarget {
	m.mu.Lock()
	keys := make([]string, 0, len(m.instances))
	instances := make(map[string]*session.Instance, len(m.instances))
	for key, instance := range m.instances {
		keys = append(keys, key)
		instances[key] = instance
	}
	m.mu.Unlock()
	sort.Strings(keys)

	targets := make([]prInfoRefreshTarget, 0, len(keys))
	for _, key := range keys {
		instance := instances[key]
		if instance == nil || instance.Capabilities().Workspace != session.WorkspaceLocalWorktree ||
			instance.IsArchived() || instance.IsTearingDown() || instance.UserKilled() ||
			instance.PRInfoAge() < m.prInfoStaleAfter {
			continue
		}
		repoPath, branch := instance.FetchPRInfoSnapshot()
		if repoPath == "" || branch == "" {
			continue
		}
		repoID, title := splitDaemonInstanceKey(key)
		instance.MarkPRInfoFetched()
		targets = append(targets, prInfoRefreshTarget{
			instance: instance,
			id:       instance.ID,
			title:    title,
			repoID:   repoID,
			repoPath: repoPath,
			branch:   branch,
		})
	}
	return targets
}

// refreshPRInfos discovers PR metadata for every eligible session and records
// it through SetPRInfo, the existing durable mutation/event path. A bounded
// worker pool prevents a large roster from launching an unbounded burst of gh
// processes, while the caller's context makes daemon shutdown cancel them.
func (m *Manager) refreshPRInfos(ctx context.Context) error {
	targets := m.prInfoRefreshTargets()
	if len(targets) == 0 {
		return nil
	}

	workers := prInfoRefreshWorkers
	if len(targets) < workers {
		workers = len(targets)
	}
	jobs := make(chan prInfoRefreshTarget)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for target := range jobs {
				m.refreshPRInfoTarget(ctx, target)
			}
		}()
	}

	for _, target := range targets {
		select {
		case jobs <- target:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	return ctx.Err()
}

func (m *Manager) refreshPRInfoTarget(ctx context.Context, target prInfoRefreshTarget) {
	info, err := m.prInfoFetcher(ctx, target.repoPath, target.branch)
	if err != nil {
		if ctx.Err() == nil {
			log.WarningLog.Printf("PR info fetch failed for %q: %v", target.title, err)
		}
		return
	}
	var data session.PRInfoData
	if info != nil {
		data = session.PRInfoData{
			Number: info.Number,
			Title:  info.Title,
			URL:    info.URL,
			State:  info.State,
		}
	}
	// Carry the lookup ref even when no PR exists. SetPRInfo uses it as a stale
	// result guard before either clearing or replacing the cached projection.
	data.Branch = target.branch
	if err := m.SetPRInfo(SetPRInfoRequest{
		ID: target.id, Title: target.title, RepoID: target.repoID, PRInfo: data,
	}); err != nil && ctx.Err() == nil && m.prInfoRefreshTargetCurrent(target) {
		log.WarningLog.Printf("failed to record PR info for %q: %v", target.title, err)
	}
}

func (m *Manager) prInfoRefreshTargetCurrent(target prInfoRefreshTarget) bool {
	m.mu.Lock()
	current := m.instances[daemonInstanceKey(target.repoID, target.title)]
	m.mu.Unlock()
	if current == nil || current.IsArchived() || current.IsTearingDown() || current.UserKilled() ||
		current.GetWorktreeBranch() != target.branch {
		return false
	}
	if target.id != "" {
		return current.ID == target.id
	}
	return current == target.instance
}

// startPRInfoRefreshLoop starts the daemon-owned PR discovery loop. The first
// pass runs immediately after restore; later passes are paced independently of
// the high-frequency liveness poll. An in-flight pass is joined on shutdown.
func startPRInfoRefreshLoop(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(prInfoRefreshInterval)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- manager.refreshPRInfos(ctx) }()
			select {
			case <-stopCh:
				cancel()
				<-done
				return
			case <-done:
				cancel()
			}
			select {
			case <-stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
}
