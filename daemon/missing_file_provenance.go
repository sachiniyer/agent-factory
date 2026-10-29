package daemon

import (
	"errors"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// missingFileProvenanceSet tracks the set of repos the daemon OBSERVED as missing
// on a prior poll, so the preservation loop keeps protecting their live in-memory
// sessions after the file is recreated as an empty "[]" (e.g. by a preserved
// session's targeted write) — without also preserving a repo that was LEGITIMATELY
// emptied (another process killed the last session, or an administrator replaced
// the roster with "[]"). A present-but-empty file is preserved only when the
// daemon previously saw it missing; a genuinely empty file the daemon never
// observed as missing is the authoritative state and its stale in-memory sessions
// are dropped. An entry is held only while the file stays missing or
// present-but-empty; once the file carries real rows again the repo recovers and
// the entry is dropped. Package-level because refreshDaemonInstances is a free
// function shared by restoreInstances and refreshLocked; entries are repo-scoped,
// so a test that reuses a repoID in a different AF home can only miss (and the
// reset helper clears it between tests).
type missingFileProvenanceSet struct {
	mu    sync.Mutex
	repos map[string]bool
}

// preservedMissingFileRepos is the singleton instance of the observed-missing
// provenance set above.
var preservedMissingFileRepos = &missingFileProvenanceSet{repos: make(map[string]bool)}

// resetPreservedMissingFileReposForTest clears the provenance set. Tests that
// stage a present-but-empty file without a prior missing-file observation must not
// inherit provenance from an earlier test that used the same repoID, so a
// missing-file episode in one test never bleeds into the legitimate-empty
// assertion of another.
func resetPreservedMissingFileReposForTest() {
	preservedMissingFileRepos.mu.Lock()
	preservedMissingFileRepos.repos = make(map[string]bool)
	preservedMissingFileRepos.mu.Unlock()
}

// observed returns a copy of the repos currently held as observed-missing.
func (p *missingFileProvenanceSet) observed() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]bool, len(p.repos))
	for repoID := range p.repos {
		out[repoID] = true
	}
	return out
}

// forget ends one repo's observed-missing episode.
func (p *missingFileProvenanceSet) forget(repoID string) {
	p.mu.Lock()
	delete(p.repos, repoID)
	p.mu.Unlock()
}

// missingRecordsRestoreBudget bounds EVERY instances-flock wait of one restore
// pass, across all repos it restores. The pass runs on the poll under m.mu, and a
// checkpoint that waited on a stuck lock holder with no deadline would hang the
// poll (and a shutdown that reached it past its stop grace, the #5844 class), so
// the budget sits well inside the 5s stop grace. A var so tests can shorten it.
var missingRecordsRestoreBudget = 2 * time.Second

// saveRestoredRepoRecords writes a repo's live in-memory sessions back to its
// instances.json, waiting at most budget for the file lock. A var so a test can
// stage success and failure without a real storage.
var saveRestoredRepoRecords = func(m *Manager, instances []*session.Instance, budget time.Duration) error {
	if m.storage == nil {
		return errNoStorageForRestore
	}
	return m.storage.SaveInstancesWithin(instances, budget)
}

var errNoStorageForRestore = errors.New("manager has no storage")

// restoreMissingRecordsLocked ends a missing-file episode by repopulating the
// durable records, not by preserving in memory forever. Preserving a repo whose
// instances.json vanished only HOLDS its sessions; the file stays gone, and the
// first targeted write (any session's persist, or a new create) recreates it
// with just that write's rows, so the other preserved sessions would be dropped
// on the next poll (#6048 review). So the poll that observes the loss writes the
// preserved sessions' records back from memory, through the same checkpoint
// path the shutdown save uses. Only after that write succeeds is the episode
// over and its provenance forgotten, which is also what keeps a LATER genuinely
// empty file — af itself writing "[]" because every session is gone, or an
// administrator replacing the roster — from being mistaken for the deleted one.
// A failed write leaves the provenance in place, so preservation continues and
// the restore is retried on the next poll. A repo with nothing left in memory
// has nothing to restore and its episode is closed. Caller holds m.mu.
func (m *Manager) restoreMissingRecordsLocked(instances map[string]*session.Instance) {
	observed := preservedMissingFileRepos.observed()
	if len(observed) == 0 {
		return
	}
	byRepo := make(map[string][]*session.Instance)
	for key, inst := range instances {
		repoID, _ := splitDaemonInstanceKey(key)
		if observed[repoID] {
			byRepo[repoID] = append(byRepo[repoID], inst)
		}
	}
	deadline := time.Now().Add(missingRecordsRestoreBudget)
	for repoID := range observed {
		insts := byRepo[repoID]
		if len(insts) == 0 {
			preservedMissingFileRepos.forget(repoID)
			continue
		}
		if err := saveRestoredRepoRecords(m, insts, max(time.Until(deadline), 0)); err != nil {
			// UNKNOWN, not failed-and-forgotten: a lock that stayed held past the
			// budget says nothing about the records, so nothing was written and
			// preservation stays ON (the provenance is kept) until a later poll
			// can take the lock.
			if errors.Is(err, config.ErrLockTimeout) {
				log.WarningLog.Printf("daemon could not take the instances lock for repo %s within %s to restore its missing records; nothing was written, the %d live session(s) stay preserved in memory, and the restore is retried on the next poll: %v", repoID, missingRecordsRestoreBudget, len(insts), err)
				continue
			}
			log.WarningLog.Printf("daemon could not restore the missing instances.json records for repo %s from %d live session(s); keeping them preserved in memory and retrying on the next poll: %v", repoID, len(insts), err)
			continue
		}
		preservedMissingFileRepos.forget(repoID)
		log.WarningLog.Printf("daemon restored the missing instances.json records for repo %s from %d live session(s)", repoID, len(insts))
	}
}
