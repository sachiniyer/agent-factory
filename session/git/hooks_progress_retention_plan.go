package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

func pruneHookProgressWithSnapshot(dir string, now time.Time, ownerSnapshot hookProgressOwnerSnapshot) error {
	var candidates []keptProgress
	var cleanups []hookProgressCleanup
	err := withHookProgressLock(dir, func(pinned string, _ os.FileInfo) error {
		if pinned != ownerSnapshot.hookDirectory {
			return fmt.Errorf("hook owner snapshot belongs to %s, not locked directory %s", ownerSnapshot.hookDirectory, pinned)
		}
		var collectErr error
		candidates, collectErr = collectHookProgressCandidatesLocked(pinned, now, ownerSnapshot.owners, &cleanups)
		return collectErr
	})
	if err != nil {
		return errors.Join(err, cleanupHookProgressArtifacts(cleanups))
	}
	return finishHookProgressPrune(dir, now, ownerSnapshot, candidates, cleanups)
}

func finishHookProgressPrune(dir string, now time.Time, ownerSnapshot hookProgressOwnerSnapshot, candidates []keptProgress, cleanups []hookProgressCleanup) error {
	// Manager discovery is deliberately outside .progress. A slow user manager
	// may delay this optional pass, but it cannot make another publisher exhaust
	// its shorter journal-lock budget.
	running, err := probeHookProgressCandidates(candidates)
	if err != nil {
		return errors.Join(err, cleanupHookProgressArtifacts(cleanups))
	}
	reclaim := selectHookProgressReclaim(candidates, running, now)
	if len(reclaim) > 0 {
		// Keep the existing final liveness recheck, also outside the lock. The
		// following lock phase revalidates every journal and lease before rename.
		running, err = probeHookProgressCandidates(reclaim)
		if err == nil {
			err = withHookProgressLock(dir, func(pinned string, _ os.FileInfo) error {
				if pinned != ownerSnapshot.hookDirectory {
					return fmt.Errorf("hook owner snapshot belongs to %s, not locked directory %s", ownerSnapshot.hookDirectory, pinned)
				}
				return retireHookProgressCandidatesLocked(now, ownerSnapshot.owners, reclaim, running, &cleanups)
			})
		}
	}
	return errors.Join(err, cleanupHookProgressArtifacts(cleanups))
}

// The caller owns .progress. This phase performs only bounded filesystem and
// lease probes; manager liveness is collected after the lock is released.
func collectHookProgressCandidatesLocked(dir string, now time.Time, owners map[string]bool, cleanups *[]hookProgressCleanup) ([]keptProgress, error) {
	entries, err := BoundedReadDir(dir)
	if err != nil {
		return nil, err
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
				return nil, readErr
			}
			var retired hookProgress
			if json.Unmarshal(data, &retired) == nil {
				receipt, receiptErr := hookReceiptDirectory(path, retired.Directory)
				if receiptErr != nil && !noResumableHookProgress(receiptErr) {
					return nil, receiptErr
				}
				if receiptErr == nil && receipt == filepath.Join(dir, strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "retired-"), ".json")) {
					if _, statErr := BoundedLstat(receipt); os.IsNotExist(statErr) {
						staleRetired = append(staleRetired, path)
					} else if statErr != nil {
						return nil, statErr
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
			return nil, err
		}
		if p.SessionID == "" || p.Prefix != systemdunit.HookScopeUnitPrefix(p.SessionID) {
			continue
		}
		retired := strings.HasPrefix(entry.Name(), "retired-entries-")
		if retired && entry.Name() != "retired-"+filepath.Base(p.Directory)+".json" {
			continue
		}
		if !retired && owners[p.SessionID] {
			continue
		}
		modified, incomplete, err := hookProgressActivity(path, p)
		if err != nil {
			return nil, err
		}
		if now.Sub(modified) < progressGraceAge {
			continue
		}
		candidates = append(candidates, keptProgress{path: path, progress: p, modified: modified, incomplete: incomplete, retired: retired})
	}
	if err := pruneUnpublishedHookReceipts(dir, entries, now, cleanups); err != nil {
		return nil, err
	}
	for _, path := range staleRetired {
		*cleanups = append(*cleanups, hookProgressCleanup{journal: path})
	}
	return candidates, nil
}

func probeHookProgressCandidates(candidates []keptProgress) (map[string]bool, error) {
	prefixes := make([]string, 0, len(candidates))
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if !seen[candidate.progress.Prefix] {
			seen[candidate.progress.Prefix] = true
			prefixes = append(prefixes, candidate.progress.Prefix)
		}
	}
	running := make(map[string]bool)
	if len(prefixes) == 0 {
		return running, nil
	}
	live, err := systemdunit.RunningHookPrefixes(prefixes...)
	if err != nil {
		return nil, err
	}
	for _, prefix := range live {
		running[prefix] = true
	}
	return running, nil
}

func selectHookProgressReclaim(candidates []keptProgress, running map[string]bool, now time.Time) []keptProgress {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].modified.Equal(candidates[j].modified) {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].modified.After(candidates[j].modified)
	})
	var reclaim []keptProgress
	kept := 0
	for _, candidate := range candidates {
		if running[candidate.progress.Prefix] {
			continue
		}
		if !candidate.retired && !candidate.incomplete && kept < keptProgressLimit && now.Sub(candidate.modified) <= keptProgressAge {
			kept++
			continue
		}
		reclaim = append(reclaim, candidate)
	}
	return reclaim
}

// Revalidate the immutable journal identity, owner class, activity age, final
// liveness snapshot and lease after reacquiring .progress. The manager probes
// that produced running happened outside this critical section.
func retireHookProgressCandidatesLocked(now time.Time, owners map[string]bool, reclaim []keptProgress, running map[string]bool, cleanups *[]hookProgressCleanup) error {
	retiredCount := 0
	for _, candidate := range reclaim {
		if running[candidate.progress.Prefix] {
			continue
		}
		current, err := readHookProgress(candidate.path)
		if err != nil {
			if noResumableHookProgress(err) {
				continue
			}
			return err
		}
		if current.Directory != candidate.progress.Directory || current.SessionID != candidate.progress.SessionID || current.Generation != candidate.progress.Generation || current.Worktree != candidate.progress.Worktree {
			continue
		}
		retired := strings.HasPrefix(filepath.Base(candidate.path), "retired-entries-")
		if !retired && owners[current.SessionID] {
			continue
		}
		modified, _, err := hookProgressActivity(candidate.path, current)
		if err != nil {
			return err
		}
		if now.Sub(modified) < progressGraceAge {
			continue
		}
		retiredPath, reclaimed, err := retireUnleasedHookProgress(candidate.path, current)
		if err != nil {
			return err
		}
		if reclaimed {
			retiredCount++
			*cleanups = append(*cleanups, hookProgressCleanup{journal: retiredPath, progress: current})
		}
	}
	if retiredCount > 0 {
		log.InfoLog.Printf("retired %d inactive hook journals", retiredCount)
	}
	return nil
}
