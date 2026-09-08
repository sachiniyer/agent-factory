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

// Match #4045's hooklog retention policy. Journals are multi-file state rather
// than output descriptors: finished receipts, absent ownership and an empty
// scope/launcher probe replace the output file's inherited descriptor lock.
const (
	keptProgressLimit = 20
	keptProgressAge   = 14 * 24 * time.Hour
	progressGraceAge  = 5 * time.Second
)

type keptProgress struct {
	path               string
	progress           *hookProgress
	modified           time.Time
	terminalIncomplete bool
}

func pruneHookProgress(dir string, now time.Time) {
	_, err := config.TryWithFileLock(filepath.Join(dir, ".progress"), func() error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if err := pruneUnpublishedHookReceipts(dir, entries, now); err != nil {
			return err
		}
		owners, err := hookProgressOwners()
		if err != nil {
			return err
		}
		var candidates []keptProgress
		for _, entry := range entries {
			if !entry.Type().IsRegular() || (!strings.HasPrefix(entry.Name(), "progress-") && !strings.HasPrefix(entry.Name(), "retired-entries-")) || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			p, err := readHookProgress(path)
			// Removal may have finished the receipt directory before the process
			// exited. The retired name cannot be adopted, so finish that deletion.
			if os.IsNotExist(err) && strings.HasPrefix(entry.Name(), "retired-entries-") {
				data, readErr := os.ReadFile(path)
				var retired hookProgress
				if readErr == nil && json.Unmarshal(data, &retired) == nil && retired.Directory == filepath.Join(dir, strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "retired-"), ".json")) {
					if _, statErr := os.Lstat(retired.Directory); os.IsNotExist(statErr) {
						_ = os.Remove(path)
					}
				}
				continue
			}
			if err != nil || p.SessionID == "" || owners[p.SessionID] || !p.finished() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			modified := info.ModTime()
			files := []string{filepath.Join(p.Directory, "finished")}
			for index := range p.Commands {
				files = append(files, filepath.Join(p.receipt(index), "exit"))
			}
			for _, path := range files {
				if info, err := os.Stat(path); err == nil && info.ModTime().After(modified) {
					modified = info.ModTime()
				}
			}
			if now.Sub(modified) < progressGraceAge {
				continue
			}
			candidates = append(candidates, keptProgress{path, p, modified, !p.completed()})
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
		removed, kept := 0, 0
		for _, candidate := range candidates {
			if !candidate.terminalIncomplete && kept < keptProgressLimit && now.Sub(candidate.modified) <= keptProgressAge {
				kept++
				continue
			}
			// A terminal marker can precede teardown. Even completed-looking receipts
			// are not sufficient evidence while a scope or delayed launcher survives.
			live, err := systemdunit.RunningHookPrefixes(candidate.progress.Prefix)
			if err != nil || len(live) != 0 {
				continue
			}
			if err := removeHookProgress(candidate.path, candidate.progress); err != nil {
				return err
			}
			removed++
		}
		if removed > 0 {
			log.InfoLog.Printf("pruned %d terminal orphan hook journals", removed)
		}
		return nil
	})
	if err != nil {
		log.WarningLog.Printf("cannot prune terminal hook journals: %v", err)
	}
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
