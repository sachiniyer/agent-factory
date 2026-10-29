package daemon

import (
	"encoding/json"
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
	// owed names the sessions (daemon instance keys) that were live when the
	// file was observed missing and whose records have not landed yet. Protection
	// is per identity, not per repo: once a session's record is back on disk its
	// later removal (another process killing it, an administrator rewriting the
	// roster) is authoritative and must not be undone (#6048 review).
	owed map[string]bool
}

// preservedMissingFileRepos is the singleton instance of the observed-missing
// provenance set above.
var preservedMissingFileRepos = &missingFileProvenanceSet{repos: make(map[string]bool), owed: make(map[string]bool)}

// resetPreservedMissingFileReposForTest clears the provenance set. Tests that
// stage a present-but-empty file without a prior missing-file observation must not
// inherit provenance from an earlier test that used the same repoID, so a
// missing-file episode in one test never bleeds into the legitimate-empty
// assertion of another.
func resetPreservedMissingFileReposForTest() {
	preservedMissingFileRepos.mu.Lock()
	preservedMissingFileRepos.repos = make(map[string]bool)
	preservedMissingFileRepos.owed = make(map[string]bool)
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

// oweLocked records that key (a live session of repoID) is owed a record after
// repoID's file was observed missing. Caller holds p.mu.
func (p *missingFileProvenanceSet) oweLocked(repoID, key string) {
	p.repos[repoID] = true
	p.owed[key] = true
}

// forgetLocked ends one repo's observed-missing episode. Caller holds p.mu.
func (p *missingFileProvenanceSet) forgetLocked(repoID string) {
	delete(p.repos, repoID)
	for key := range p.owed {
		if id, _ := splitDaemonInstanceKey(key); id == repoID {
			delete(p.owed, key)
		}
	}
}

// forget ends one repo's observed-missing episode.
func (p *missingFileProvenanceSet) forget(repoID string) {
	p.mu.Lock()
	p.forgetLocked(repoID)
	p.mu.Unlock()
}

// settle marks the sessions whose records landed (by title) as no longer owed,
// and ends the episode when nothing is owed any more.
func (p *missingFileProvenanceSet) settle(repoID string, landedTitles []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, title := range landedTitles {
		delete(p.owed, daemonInstanceKey(repoID, title))
	}
	for key := range p.owed {
		if id, _ := splitDaemonInstanceKey(key); id == repoID {
			return
		}
	}
	p.forgetLocked(repoID)
}

// owedSnapshotLocked copies the owed keys. Caller holds p.mu.
func (p *missingFileProvenanceSet) owedSnapshotLocked() map[string]bool {
	out := make(map[string]bool, len(p.owed))
	for key := range p.owed {
		out[key] = true
	}
	return out
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
//
// It returns the titles whose records are on disk afterwards, because the
// checkpoint projection deliberately omits in-flight rows (a Deleting or pre-push
// archive, a session still Loading): a successful save does not mean every
// session landed.
var saveRestoredRepoRecords = func(m *Manager, repoID string, instances []*session.Instance, budget time.Duration) ([]string, error) {
	if m.storage == nil {
		return nil, errNoStorageForRestore
	}
	if err := m.storage.SaveInstancesWithin(instances, budget); err != nil {
		return nil, err
	}
	// Read the restored rows back through LoadRepoInstances directly, not
	// config.LoadState().GetInstances: LoadState() saves a default state.json
	// when the file is missing, and SaveState() takes an unbounded WithFileLock
	// (config.WithFileLock). This restore runs inside refreshLocked under m.mu,
	// so a peer holding state.json.lock would freeze every repo's snapshot and
	// session mutation and block shutdown from draining the poll. GetInstances
	// only delegates to LoadRepoInstances, so reading the repo file directly
	// avoids the state-file lock entirely (#6048 review).
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	titles := make([]string, 0, len(rows))
	for _, r := range rows {
		titles = append(titles, r.Title)
	}
	return titles, nil
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
		landed, err := saveRestoredRepoRecords(m, repoID, insts, max(time.Until(deadline), 0))
		if err != nil {
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
		onDisk := make(map[string]bool, len(landed))
		for _, title := range landed {
			onDisk[title] = true
		}
		var pending []string
		for _, inst := range insts {
			if !onDisk[inst.Title] {
				pending = append(pending, inst.Title)
			}
		}
		if len(pending) > 0 {
			// The checkpoint omitted these in-flight rows; keep protecting THEM
			// (and only them) until their record lands: the sessions whose records
			// did land are settled, so removing one later is respected.
			preservedMissingFileRepos.settle(repoID, landed)
			log.WarningLog.Printf("daemon restored the missing instances.json records for repo %s except %d in-flight session(s) the checkpoint cannot write yet (%v); they stay preserved in memory until their records land", repoID, len(pending), pending)
			continue
		}
		preservedMissingFileRepos.forget(repoID)
		log.WarningLog.Printf("daemon restored the missing instances.json records for repo %s from %d live session(s)", repoID, len(insts))
	}
}

// landedLocked reports whether every session still owed a record for repoID has
// it in the freshly loaded set (next is built from disk rows at the call site),
// or has left memory so nothing is owed for it. The restore checkpoint cannot
// write every live session — it omits in-flight rows such as a Deleting or
// pre-push archive (#6048 review) — so a successful write alone must not end the
// episode: protection lasts until each owed session is durably back. Caller holds
// p.mu.
func (p *missingFileProvenanceSet) landedLocked(repoID string, existing, next map[string]*session.Instance) bool {
	for key := range p.owed {
		if id, _ := splitDaemonInstanceKey(key); id == repoID && existing[key] != nil && next[key] == nil {
			return false
		}
	}
	return true
}
