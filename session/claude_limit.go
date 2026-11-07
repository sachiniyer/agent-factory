package session

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/handoff"
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
//   - Directory scoping cannot attribute records inside a CARRIED file: an
//     account swap copies the conversation transcript wholesale into the new
//     account's home, so the first carried-prefix bytes are the swapped-away
//     account's records. The durable watermark the carry writes
//     (handoff.State.CarriedPrefixes, merged with the unrecorded-carry
//     quarantine) bounds each file's eligible region — a claim below it is
//     the source account's wall and is skipped (#6944 review).
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
// describes, read under exactly that carried-prefix watermark. A file that
// grew or changed since the memo re-scans; an unchanged file replays its
// verdict without re-decoding its tail. The watermark is part of the key
// because an account carry changes which bytes the scan may trust WITHOUT
// touching the file — a carry landing between polls must re-scan (#6944
// review).
type claudeLimitScanEntry struct {
	modTime   time.Time
	size      int64
	watermark int64
	claim     claudeLimitClaim
}

// claudeLimitCarriedPrefixes reads the durable carried-prefix watermarks:
// the first watermark bytes of a carried transcript are the SOURCE
// account's records, not the current account's — an account swap carries
// the conversation file wholesale, so directory scoping alone cannot
// attribute the records (#6944 review). A missing store is an empty map; a
// corrupt one fails closed — the scan cannot prove record provenance, so
// every file reads as fully carried rather than one carried byte leaking
// through.
func claudeLimitCarriedPrefixes() (map[string]int64, error) {
	store, err := handoff.DefaultStore()
	if err != nil {
		return nil, err
	}
	railsStore, err := handoff.DefaultRailsStore()
	if err != nil {
		return nil, err
	}
	snap, stateErr, railsErr := handoff.LoadSnapshot(store, railsStore)
	if stateErr != nil {
		return nil, stateErr
	}
	if railsErr != nil {
		return nil, railsErr
	}
	return snap.EffectiveCarriedPrefixes(), nil
}

// newestClaudeLimitClaim returns the freshest refusal claim across the
// candidate transcript directories, memoizing each file's claim by
// fingerprint so an unchanged pane tick does not re-read every tail.
func (i *Instance) newestClaudeLimitClaim(dirs []string) (claudeLimitClaim, bool) {
	prefixes, prefixesErr := claudeLimitCarriedPrefixes()
	// A carry that predates Claude watermarks left no durable boundary at
	// all — but the session row durably marks the conversation it carries
	// (ConversationCaptureCarried). Its transcript file cannot attribute
	// ANY record to either account: below the unknown boundary is the
	// source's refusal, above it the target's appends — with no mark the
	// two are indistinguishable, so the whole file reads as carried.
	carriedName := ""
	if conv := i.AgentConversation(); conv.CaptureKind == ConversationCaptureCarried && conv.ID != "" {
		carriedName = conv.ID + ".jsonl"
	}
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
			// A failed watermark load reads as "everything is carried
			// poison": the file may still hold the source account's
			// refusal, so no byte in it can prove a reset.
			watermark := int64(math.MaxInt64)
			if prefixesErr == nil {
				watermark = prefixes[filepath.Clean(path)]
				if watermark == 0 && carriedName != "" && filepath.Base(path) == carriedName {
					watermark = math.MaxInt64
				}
			}
			if !ok || !entry.modTime.Equal(info.ModTime()) || entry.size != info.Size() ||
				entry.watermark != watermark {
				claim, err := transcriptLimitClaim(path, info.ModTime(), watermark)
				if err != nil {
					continue
				}
				entry = claudeLimitScanEntry{
					modTime: info.ModTime(), size: info.Size(), watermark: watermark, claim: claim,
				}
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
// timestamp is unreadable. The eligible region is [watermark, size): bytes
// below the carried-prefix watermark are the SOURCE account's records — a
// refusal recorded before the file carried cannot supply the new account's
// reset.
func transcriptLimitClaim(path string, mtime time.Time, watermark int64) (claudeLimitClaim, error) {
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
	if watermark > offset {
		offset = watermark
	}
	if offset >= info.Size() {
		return claudeLimitClaim{verdict: quota.ClaudeLimitAbsent}, nil
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return claudeLimitClaim{}, err
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return claudeLimitClaim{}, err
	}
	lines := bytes.Split(tail, []byte{'\n'})
	if offset > 0 && offset != watermark {
		// A window opening mid-file begins inside a line — the first
		// fragment is not a record start and is dropped. Opening exactly
		// AT the watermark is different: the watermark is a record
		// boundary by contract, so the first line IS a complete record
		// and stays — the same rule internal/rateindex applies.
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
