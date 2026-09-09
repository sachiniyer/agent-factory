package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

// Hook progress retention has four cooperating authorities. A live owner row
// protects every resumable journal; an archived or tombstoned row is terminal
// and therefore permits eventual reclamation. A runner or pending create holds
// runner.lock across gaps where no scope is visible, and every nested runner
// owns exactly one lease hold. Retirement first removes the resumable journal
// name, then deletes receipts; a retired name can never be adopted and is safe
// to finish reclaiming after the shared liveness probes and lease check. Unknown
// ownership, metadata, or manager liveness leaves all affected state in place.
// The age and count limits otherwise match #4045's hooklog retention policy.
const (
	keptProgressLimit = 20
	keptProgressAge   = 14 * 24 * time.Hour
	progressGraceAge  = 5 * time.Second
)

type keptProgress struct {
	path       string
	progress   *hookProgress
	modified   time.Time
	incomplete bool
	retired    bool
}

func pruneHookProgress(dir string, now time.Time) {
	err := withHookProgressLock(dir, func(pinned string, _ os.FileInfo) error { return pruneHookProgressLocked(pinned, now) })
	if err != nil {
		log.WarningLog.Printf("cannot prune inactive hook journals: %v", err)
	}
}

// The caller owns .progress, whether pruning alone or just before publication.
func pruneHookProgressLocked(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	owners, err := hookProgressOwners()
	if err != nil {
		return err
	}
	var candidates []keptProgress
	var staleRetired []string
	for _, entry := range entries {
		if !entry.Type().IsRegular() || (!strings.HasPrefix(entry.Name(), "progress-") && !strings.HasPrefix(entry.Name(), "retired-entries-")) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		p, err := readHookProgress(path)
		// Removal may have finished the receipt directory before the process
		// exited. The retired name cannot be adopted, so finish that deletion.
		if os.IsNotExist(err) && strings.HasPrefix(entry.Name(), "retired-entries-") {
			data, readErr := BoundedReadFile(path)
			if readErr != nil {
				return readErr
			}
			var retired hookProgress
			if json.Unmarshal(data, &retired) == nil {
				receipt, receiptErr := hookReceiptDirectory(path, retired.Directory)
				if receiptErr != nil && !noResumableHookProgress(receiptErr) {
					return receiptErr
				}
				if receiptErr == nil && receipt == filepath.Join(dir, strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "retired-"), ".json")) {
					if _, statErr := BoundedLstat(receipt); os.IsNotExist(statErr) {
						staleRetired = append(staleRetired, path)
					} else if statErr != nil {
						return statErr
					}
				}
			}
			continue
		}
		if err != nil {
			// Absence and structurally invalid JSON are conclusive for this journal,
			// so independent valid journals can still be reclaimed. I/O failures and
			// timeouts are a third answer: abort before paying another per-path
			// deadline or deleting anything based on an incomplete view.
			if noResumableHookProgress(err) {
				continue
			}
			return err
		}
		if p.SessionID == "" || p.Prefix != systemdunit.HookScopeUnitPrefix(p.SessionID) {
			continue
		}
		retired := strings.HasPrefix(entry.Name(), "retired-entries-")
		if retired && entry.Name() != "retired-"+filepath.Base(p.Directory)+".json" {
			continue // A retired artifact must name its own receipt directory.
		}
		activeOwner := owners[p.SessionID]
		// Active rows remain resumable. Archived and tombstoned rows are
		// terminal ownership evidence, so their unfinished journals join the
		// same grace/liveness/lease reclamation path as ownerless journals.
		if !retired && activeOwner {
			continue
		}
		modified, incomplete, err := hookProgressActivity(path, p)
		if err != nil {
			return err
		}
		if now.Sub(modified) < progressGraceAge {
			continue
		}
		candidates = append(candidates, keptProgress{path: path, progress: p, modified: modified, incomplete: incomplete, retired: retired})
	}
	// Receipt discovery can also need metadata. Do it before deleting any
	// journal so an inconclusive filesystem answer aborts this pass intact.
	if err := pruneUnpublishedHookReceipts(dir, entries, now); err != nil {
		return err
	}
	for _, path := range staleRetired {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// Exclude active scopes from the quota as well as deletion. One fleet
	// probe also protects terminal-marked journals whose teardown is pending.
	var prefixes []string
	for _, candidate := range candidates {
		prefixes = append(prefixes, candidate.progress.Prefix)
	}
	if len(prefixes) > 0 {
		live, probeErr := systemdunit.RunningHookPrefixes(prefixes...)
		if probeErr != nil {
			return probeErr
		}
		running := make(map[string]bool)
		for _, prefix := range live {
			running[prefix] = true
		}
		eligible := candidates[:0]
		for _, candidate := range candidates {
			if !running[candidate.progress.Prefix] {
				eligible = append(eligible, candidate)
			}
		}
		candidates = eligible
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].modified.Equal(candidates[j].modified) {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].modified.After(candidates[j].modified)
	})
	var reclaim []keptProgress
	kept := 0
	for _, candidate := range candidates {
		if !candidate.retired && !candidate.incomplete && kept < keptProgressLimit && now.Sub(candidate.modified) <= keptProgressAge {
			kept++
			continue
		}
		reclaim = append(reclaim, candidate)
	}
	// Recheck the final deletion set once, not once per journal: each
	// manager outage must consume a constant number of probe timeouts
	// while this home-wide publication lock is held.
	if len(reclaim) == 0 {
		return nil
	}
	prefixes = nil
	for _, candidate := range reclaim {
		prefixes = append(prefixes, candidate.progress.Prefix)
	}
	live, err := systemdunit.RunningHookPrefixes(prefixes...)
	if err != nil {
		return err
	}
	running := make(map[string]bool)
	for _, prefix := range live {
		running[prefix] = true
	}
	removed := 0
	for _, candidate := range reclaim {
		if running[candidate.progress.Prefix] {
			continue
		}
		reclaimed, err := pruneUnleasedHookProgress(candidate.path, candidate.progress)
		if err != nil {
			return err
		}
		if reclaimed {
			removed++
		}
	}
	if removed > 0 {
		log.InfoLog.Printf("pruned %d inactive hook journals", removed)
	}
	return nil
}

func hookProgressActivity(path string, p *hookProgress) (time.Time, bool, error) {
	info, err := BoundedLstat(path)
	if err != nil {
		return time.Time{}, false, err
	}
	modified, complete := info.ModTime(), true
	files := []struct {
		path     string
		required bool
	}{{path: p.Directory}, {path: filepath.Join(p.Directory, "finished"), required: true}}
	for index := range p.Commands {
		files = append(files,
			struct {
				path     string
				required bool
			}{path: p.receipt(index)},
			struct {
				path     string
				required bool
			}{path: filepath.Join(p.receipt(index), "exit"), required: true},
		)
	}
	for _, file := range files {
		info, err := BoundedLstat(file.path)
		if os.IsNotExist(err) {
			if file.required {
				complete = false
			}
			continue
		}
		if err != nil {
			return time.Time{}, false, err
		}
		if file.required && !info.Mode().IsRegular() {
			complete = false
		}
		if info.ModTime().After(modified) {
			modified = info.ModTime()
		}
	}
	return modified, !complete, nil
}

func hookProgressOwners() (map[string]bool, error) {
	repos, skipped, err := config.LoadAllRepoInstancesReportingSkipDetails()
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return nil, skipped[0].Err
	}
	owners := make(map[string]bool)
	for _, data := range repos {
		var rows []struct {
			ID         string `json:"id"`
			Liveness   int    `json:"liveness"`
			Status     int    `json:"status"`
			UserKilled bool   `json:"user_killed"`
		}
		if err := json.Unmarshal(data, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			// Append-only persisted enums: session.LiveArchived=5 and
			// legacy session.Archived=6. The git package cannot import session.
			archived := row.Liveness == 5 || (row.Liveness == 0 && row.Status == 6)
			owners[row.ID] = owners[row.ID] || (!archived && !row.UserKilled)
		}
	}
	return owners, nil
}
