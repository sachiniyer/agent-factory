package session

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var codexRolloutFileRE = regexp.MustCompile(`(?i)^rollout-.+-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// codexRolloutStoreLookback is how far behind a capture snapshot the bounded
// scan still descends. Codex files a rollout under sessions/YYYY/MM/DD named
// from the recorder's LOCAL calendar date (codex-rs rollout recorder
// precompute_new_rollout_path: OffsetDateTime::now_local), while the snapshot
// stamps af's own clock — the same instant can land on dates ~26h apart
// across the two, and the clocks themselves can be skewed. 48h covers the
// worst combination with margin while still excluding every older day
// directory in the store (#6845).
const codexRolloutStoreLookback = 48 * time.Hour

// codexRolloutScanFloor returns the earliest instant a rollout belonging to a
// capture armed at startedAt could possibly have been written.
func codexRolloutScanFloor(startedAt time.Time) time.Time {
	return startedAt.Add(-codexRolloutStoreLookback)
}

// codexRolloutFiles enumerates every rollout file under codexHome's sessions
// store — the unbounded form for callers that can legitimately touch a
// rollout of any age: pinning an explicit `codex resume` id
// (CodexRolloutPathByID), sizing the unpinned resume pool
// (TrackCodexReceiptPool), checking a recorded conversation for staleness, or
// locating a carried rollout across an account swap.
func codexRolloutFiles(codexHome string) (map[string]struct{}, error) {
	return codexRolloutFilesSince(codexHome, time.Time{})
}

// codexRolloutFilesSince enumerates the store like codexRolloutFiles but
// never descends into a sessions/YYYY/MM/DD directory whose date provably
// predates floor's date: the recorder names that directory from its local
// clock at mint time, so the date is a hard bound on how early anything
// beneath it was written. The daemon's capture loop re-runs this scan on
// every rearm for the life of an unrecorded session — the unbounded walk
// that made it ~12.5% of daemon allocations on a well-used store (#6845).
// A zero floor walks the whole store.
//
// The fail-closed rule is unchanged: a directory the scan still needs but
// cannot traverse is an error, never an empty-looking store (#5471). A
// directory outside the window is never opened, so its unreadability cannot
// fail the scan either.
func codexRolloutFilesSince(codexHome string, floor time.Time) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if codexHome == "" {
		return out, nil
	}
	root := filepath.Join(codexHome, "sessions")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// An entry vanishing mid-walk is normal; a store that cannot be
			// traversed is NOT an empty store. Swallowing the failure would let
			// a receipt check report never-arrived for a directory it never
			// inspected (#5471 review).
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d == nil {
			return nil
		}
		if d.IsDir() {
			if rel, relErr := filepath.Rel(root, path); relErr == nil &&
				codexRolloutDirBeforeFloor(rel, floor) {
				return filepath.SkipDir
			}
			return nil
		}
		if codexConversationIDFromPath(path) == "" {
			return nil
		}
		out[path] = struct{}{}
		return nil
	})
	return out, err
}

// codexRolloutDirBeforeFloor reports whether rel — a directory's path
// relative to the sessions root — is a complete YYYY/MM/DD date predating
// floor's date. Only the strict leaf proves it: the recorder writes
// zero-padded four/two/two-digit levels, so shallower levels, deeper
// descendants, unpadded fields, and malformed or impossible dates all prove
// nothing, and the walk descends into each of them rather than guess.
// Comparison is civil-day against civil-day so the floor's own date always
// stays inside the window.
func codexRolloutDirBeforeFloor(rel string, floor time.Time) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || len(parts[0]) != 4 || len(parts[1]) != 2 || len(parts[2]) != 2 {
		return false
	}
	day, err := time.ParseInLocation("2006/01/02", strings.Join(parts, "/"), floor.Location())
	if err != nil {
		return false
	}
	y, m, d := floor.Date()
	return day.Before(time.Date(y, m, d, 0, 0, 0, 0, floor.Location()))
}

func codexConversationIDFromPath(path string) string {
	matches := codexRolloutFileRE.FindStringSubmatch(filepath.Base(path))
	if len(matches) != 2 {
		return ""
	}
	return strings.ToLower(matches[1])
}
