package session

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/quota"
)

// This file is the structural half of #6944: when Claude's usage-limit banner
// wraps in a narrow pane, the pane-derived reset can pick the wrong timezone —
// or no timezone at all — and the session parks to auto-resume at the wrong
// instant. Claude's own transcript settles it without any pane text: a refused
// request is recorded with apiError="usage_limit_reached" and the wall's reset
// instant in epoch seconds. The pane stays the detection trigger; this method
// is the authoritative reset source whenever the transcript carries the
// refusal record, and scoping is what keeps it honest:
//
//   - ClaudeRunTranscriptDirs already answers for the session's CURRENT
//     account (agentaccount.Selected) and launch directory, so a transcript
//     from an account the session swapped away from — or a different
//     worktree's project — is never a candidate (Captain's #6944 adjustment).
//   - Usage limits are per-account, so the freshest refusal record across
//     the candidate transcripts IS the wall's freshest statement; ordering
//     is by the record's own timestamp, which file position cannot supply.
//   - A malformed claim (the record's shape but unreadable fields) supplies
//     nothing — "unknown" — rather than a guessed instant, and a stale or
//     reset-less freshest claim supplies nothing rather than resurrecting an
//     older wall.
//
// Cost: the poll consults this on every unchanged-pane tick while a banner
// shows, and the sweep is serial — so per-file claims are memoized on the
// instance by file fingerprint (mtime+size), and only a transcript that
// actually grew or changed is re-read (#6975 review).

// claudeLimitTailBytes bounds the scan to the transcript's tail: a refusal
// record current enough to describe the pane's live wall was appended
// recently, and transcripts grow unboundedly otherwise. Generous against
// multi-KB records so a refusal a few records back still lands inside.
const claudeLimitTailBytes = 512 * 1024

// ClaudeActiveLimitReset returns the reset instant the session's Claude
// transcripts prove for the wall the pane is showing, or ok=false when they
// prove none — no refusal record, the freshest claim expired or malformed or
// reset-less, a non-claude session, or no transcript access at all. now is
// the caller's clock: a refusal whose resetsAt already passed is a wall that
// ended, not a wall (the same compare Claude itself applies before treating
// a recorded limit as current).
func (i *Instance) ClaudeActiveLimitReset(now time.Time) (time.Time, bool) {
	_, dirs, err := i.ClaudeRunTranscriptDirs()
	if err != nil {
		return time.Time{}, false
	}
	claim, ok := i.newestClaudeLimitClaim(dirs)
	if !ok || claim.verdict != quota.ClaudeLimitParsed ||
		claim.record.ResetsAt == nil || !claim.record.ResetsAt.After(now) {
		return time.Time{}, false
	}
	return *claim.record.ResetsAt, true
}

// claudeLimitClaim is one transcript's freshest usage-limit statement.
type claudeLimitClaim struct {
	verdict quota.ClaudeLimitVerdict
	record  quota.ClaudeLimitRecord
	// sortAt orders claims across files and inside one file: the record's
	// own timestamp for parsed claims, its extracted timestamp (or the
	// file's mtime) for malformed ones — a malformed record can still carry
	// a valid ordering key, and an old malformed claim must not shadow the
	// newer claims its file keeps accruing.
	sortAt time.Time
}

// claudeLimitScanEntry is the memoized claim for one transcript at one file
// fingerprint — the decode result for exactly the bytes (modTime, size)
// describes. A file that grew or changed since the memo re-scans; an
// unchanged file replays its verdict without re-decoding its tail.
type claudeLimitScanEntry struct {
	modTime time.Time
	size    int64
	claim   claudeLimitClaim
}

// newestClaudeLimitClaim returns the freshest refusal claim across the
// candidate transcript directories, memoizing each file's claim by
// fingerprint so an unchanged pane tick does not re-read every tail.
func (i *Instance) newestClaudeLimitClaim(dirs []string) (claudeLimitClaim, bool) {
	i.claudeLimitScanMu.Lock()
	defer i.claudeLimitScanMu.Unlock()
	cache := i.claudeLimitScan
	if cache == nil {
		cache = make(map[string]claudeLimitScanEntry)
	}
	live := make(map[string]claudeLimitScanEntry, len(cache))
	var best claudeLimitClaim
	found := false
	for _, dir := range dirs {
		transcripts, err := claudeProjectTranscripts(dir)
		if err != nil {
			continue
		}
		for _, transcript := range transcripts {
			path := filepath.Join(dir, transcript.name)
			entry, ok := cache[path]
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if !ok || !entry.modTime.Equal(info.ModTime()) || entry.size != info.Size() {
				claim, err := transcriptLimitClaim(path, info.ModTime())
				if err != nil {
					continue
				}
				entry = claudeLimitScanEntry{modTime: info.ModTime(), size: info.Size(), claim: claim}
			}
			live[path] = entry
			if entry.claim.verdict == quota.ClaudeLimitAbsent {
				continue
			}
			if !found || entry.claim.sortAt.After(best.sortAt) {
				best, found = entry.claim, true
			}
		}
	}
	i.claudeLimitScan = live
	return best, found
}

// transcriptLimitClaim reads the transcript's tail and returns its freshest
// refusal claim by record timestamp, or the absent claim when the tail holds
// none. mtime is the ordering fallback for a malformed claim whose own
// timestamp is unreadable.
func transcriptLimitClaim(path string, mtime time.Time) (claudeLimitClaim, error) {
	f, err := os.Open(path)
	if err != nil {
		return claudeLimitClaim{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return claudeLimitClaim{}, err
	}
	offset := int64(0)
	if size := info.Size(); size > claudeLimitTailBytes {
		offset = size - claudeLimitTailBytes
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return claudeLimitClaim{}, err
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return claudeLimitClaim{}, err
	}
	lines := bytes.Split(tail, []byte{'\n'})
	if offset > 0 {
		// The window opened mid-line: the first fragment is not a record.
		lines = lines[1:]
	}
	claim := claudeLimitClaim{verdict: quota.ClaudeLimitAbsent}
	for _, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		verdict, record := quota.ParseClaudeLimitRecordLine(line)
		var sortAt time.Time
		switch verdict {
		case quota.ClaudeLimitParsed:
			sortAt = record.RecordedAt
		case quota.ClaudeLimitMalformed:
			var ok bool
			if sortAt, ok = claudeClaimTimestamp(line); !ok {
				sortAt = mtime
			}
		default:
			continue
		}
		if claim.verdict == quota.ClaudeLimitAbsent || !sortAt.Before(claim.sortAt) {
			claim = claudeLimitClaim{verdict: verdict, record: record, sortAt: sortAt}
		}
	}
	return claim, nil
}

// claudeClaimTimestamp extracts a transcript line's own timestamp to order a
// malformed claim — the refusal shape may be wrong while the record's
// timestamp still sorts.
func claudeClaimTimestamp(line []byte) (time.Time, bool) {
	var rec struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}
