package handoff

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// This file owns auto-handoff-rails.json — the durable records v1.0.345
// does NOT model. v1.0.345 rewrites the shared state file on every codex
// carry and emits only the fields it knows, so anything kept there would
// be silently dropped on downgrade (#6776 review): the rails get their own
// file, which the release never opens. The shared file keeps exactly
// v1.0.345's field set — watermarks and the halt — so the downgrade story
// is lossless in both directions.
//
// A pre-split in-flight build wrote these same records into the state
// file. Reads handle that upgrade path: when no rails file exists the
// mover keys are LIFTED from the state file at load time, and the next
// mutation writes them here and strips them there — one locked pass, no
// operator step.

// RailsFileName is the rails file's name under <af-home>/instances/.
const RailsFileName = "auto-handoff-rails.json"

const railsSchemaVersion = 1

// Rails is the durable record the mover reads and writes that the released
// binary does not know. Fields are additive: readers tolerate fields a
// newer slice writes, and every field must be meaningful when absent
// (omitempty everywhere) so a file written by an older af still decodes.
type Rails struct {
	SchemaVersion int `json:"schema_version"`
	// RecentMoves are the committed automatic moves the switch-loop rail
	// (max_moves inside max_moves_window, counted per session_key) and the
	// anti-pile-on credits (counted per target agent + account) read.
	// Written by the live-mover slice; shadow reads them as zero when
	// absent.
	RecentMoves []RecentMove `json:"recent_moves,omitempty"`
	// RefusedUntil maps stableSessionKey → the instant until which the
	// engine leaves the session alone after it refused every candidate.
	RefusedUntil map[string]time.Time `json:"refused_until,omitempty"`
	// ReportsHeld records a report_to backlog whose delivery has been held
	// past the warn threshold — a recipient whose composer never proves
	// empty must not stay silent (#6818 final review). Written once per
	// held episode by the report worker; cleared when the backlog drains
	// or is dropped. `af quota` surfaces it on the mover line.
	ReportsHeld *ReportsHeld `json:"reports_held,omitempty"`
	// UnrecordedCarries maps a rollout file's absolute path to the time a
	// carry copied source-account bytes into it WITHOUT recording the
	// watermark — the record could not land (corrupt file, unwritable
	// home) but the carry itself still committed (#6776 review: an
	// unreadable watermark file must never force a fresh conversation).
	// Readers treat the file's ENTIRE contents as carried poison: no claim
	// from it may free a wall, which is exactly "the target account's
	// readings are untrustworthy for retraction". The entry retires when a
	// later carry records that file's watermark for real.
	UnrecordedCarries map[string]time.Time `json:"unrecorded_carries,omitempty"`
}

// ReportsHeld is the durable record that a report_to delivery is held:
// Since is when the hold began — the first writer wins, so it is the
// oldest standing hold.
type ReportsHeld struct {
	Since time.Time `json:"since"`
}

// RecentMove is one committed automatic account move: when, and to which
// agent + account. Decay is the reader's job — entries outside
// max_moves_window simply count for nothing.
type RecentMove struct {
	SessionKey string `json:"session_key"`
	// Agent namespaces the account — "work" on codex is a different seat
	// than "work" on claude, and the anti-pile-on credit counts occupancy
	// per agent-namespace just like the emptiest tiebreak (#6776 review).
	// Empty on records written before the field existed; they match no
	// agent and simply never count.
	Agent   string    `json:"agent,omitempty"`
	Account string    `json:"account"`
	At      time.Time `json:"at"`
}

// railsKeys are the top-level fields this binary models in the rails file;
// a read-modify-write preserves every other key verbatim.
var railsKeys = map[string]bool{
	"schema_version":     true,
	"recent_moves":       true,
	"refused_until":      true,
	"reports_held":       true,
	"unrecorded_carries": true,
}

// liftedStateKeys are the mover records a pre-split build left in the
// shared state file — lifted at load when no rails file exists yet, then
// stripped from there by the next rails mutation.
var liftedStateKeys = map[string]bool{
	"recent_moves":       true,
	"refused_until":      true,
	"reports_held":       true,
	"unrecorded_carries": true,
}

// unrecordedCarryCap bounds the quarantine list: a home whose watermark
// file stays unwritable must not grow a second file without limit. Entries
// past the cap retire oldest-first — a retired entry re-quarantines on the
// next unrecorded carry of that path.
const unrecordedCarryCap = 256

// Snapshot is one consistent read of both durable files — the shared
// v1.0.345-compatible state plus the newer rails.
type Snapshot struct {
	State State
	Rails Rails
}

// EffectiveCarriedPrefixes merges the recorded watermarks with the
// unrecorded-carry quarantine: a carried file whose watermark never landed
// is treated as carried end-to-end (MaxInt64), so no claim inside it can
// free a wall. The marker always wins over a recorded watermark — the
// watermark may describe an OLDER carry's boundary while the file has
// since taken fresh source bytes past it. The marker retires only when a
// successful watermark write explicitly clears it.
func (s Snapshot) EffectiveCarriedPrefixes() map[string]int64 {
	var out map[string]int64
	for path, n := range s.State.CarriedPrefixes {
		if out == nil {
			out = map[string]int64{}
		}
		out[path] = n
	}
	for path := range s.Rails.UnrecordedCarries {
		if out == nil {
			out = map[string]int64{}
		}
		out[path] = math.MaxInt64
	}
	return out
}

// LoadSnapshot reads both durable record files into one Snapshot whose
// carried-prefix exclusions are fail-closed under a concurrent carry
// commit. A naive state-then-rails pair can interleave with that commit —
// a state-file watermark write followed by a rails-file marker clear —
// and pair the retired marker's absence with the watermark it superseded:
// bytes the commit just disowned then count toward the target's wall.
// Reading each file a second time and merging worst-case closes the
// window: watermarks merge by per-path MAXIMUM and quarantine markers by
// UNION, so an interleaved commit can only ever over-exclude claims —
// never count bytes a completed carry disowned. Every other field takes
// the later read; each lives in exactly one file, where the second load
// is simply the fresher consistent cut.
//
// stateErr and railsErr report each file's decode independently: a caller
// that surfaces per-file unreadability keeps both, while a fail-closed
// caller (engine, usage proof) collapses them into one error. On a decode
// failure the Snapshot still carries whichever reads succeeded, for
// callers that degrade rather than fail.
func LoadSnapshot(store *Store, railsStore *RailsStore) (snap Snapshot, stateErr, railsErr error) {
	return loadSnapshot(store.Load, railsStore.Load)
}

// loadSnapshot is LoadSnapshot's body behind injectable reads — the tests
// script a commit landing between the loads, which no on-disk fixture can
// reproduce deterministically.
func loadSnapshot(loadState func() (State, error), loadRails func() (Rails, error)) (Snapshot, error, error) {
	r1, railsErr := loadRails()
	s1, stateErr := loadState()
	snap := Snapshot{State: s1, Rails: r1}
	if stateErr != nil || railsErr != nil {
		return snap, stateErr, railsErr
	}
	r2, err := loadRails()
	if err != nil {
		// A file that decoded a moment ago but not now is unreadable for
		// the caller to surface; the earlier consistent read still fills
		// the returned snapshot.
		return snap, nil, err
	}
	s2, err := loadState()
	if err != nil {
		return snap, err, nil
	}
	snap = Snapshot{State: s2, Rails: r2}
	for path, n := range s1.CarriedPrefixes {
		if n > snap.State.CarriedPrefixes[path] {
			if snap.State.CarriedPrefixes == nil {
				snap.State.CarriedPrefixes = map[string]int64{}
			}
			snap.State.CarriedPrefixes[path] = n
		}
	}
	for path, marked := range r1.UnrecordedCarries {
		if _, ok := snap.Rails.UnrecordedCarries[path]; !ok {
			if snap.Rails.UnrecordedCarries == nil {
				snap.Rails.UnrecordedCarries = map[string]time.Time{}
			}
			snap.Rails.UnrecordedCarries[path] = marked
		}
	}
	return snap, nil, nil
}

// RailsStore reads and mutates one auto-handoff-rails file. statePath is
// the shared state file's location — the lift source for pre-split homes.
type RailsStore struct {
	path      string
	statePath string
}

// DefaultRailsPath resolves <af-home>/instances/auto-handoff-rails.json.
func DefaultRailsPath() (string, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "instances", RailsFileName), nil
}

// DefaultRailsStore opens the rails store under the default af home.
func DefaultRailsStore() (*RailsStore, error) {
	rails, err := DefaultRailsPath()
	if err != nil {
		return nil, err
	}
	state, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return &RailsStore{path: rails, statePath: state}, nil
}

// NewRailsStore opens the store at explicit paths (tests, non-default
// homes): the rails file plus the state file it lifts pre-split records
// from.
func NewRailsStore(path, statePath string) *RailsStore {
	return &RailsStore{path: path, statePath: statePath}
}

// Path is the file this store reads and writes.
func (s *RailsStore) Path() string {
	return s.path
}

// Load reads the rails file. A missing file lifts any mover records a
// pre-split build left in the state file — records are never lost to the
// split. A CORRUPT file (either one) errors: fail closed, never rebuild.
func (s *RailsStore) Load() (Rails, error) {
	rails, _, err := s.loadMerged()
	return rails, err
}

// loadMerged returns the decoded rails plus the raw fields a preserving
// rewrite needs. When the rails file does not exist the answer comes from
// lifted state-file keys — decoded under the SAME validation, so a corrupt
// pre-split record fails closed here too.
func (s *RailsStore) loadMerged() (Rails, map[string]json.RawMessage, error) {
	raw, exists, err := readFileNoFollow(s.path)
	if err != nil {
		return Rails{}, nil, err
	}
	if exists {
		rails, err := decodeRails(s.path, raw)
		if err != nil {
			return Rails{}, nil, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return Rails{}, nil, fmt.Errorf("decode %s: %w", s.path, err)
		}
		return rails, fields, nil
	}
	lifted, err := s.lift()
	if err != nil {
		return Rails{}, nil, err
	}
	return lifted, nil, nil
}

// lift reads mover keys out of the shared state file for a home whose
// rails file has never been written. An absent state file is an empty
// answer; a corrupt one fails closed — a pre-split record is still a
// record, and guessing at it is not allowed.
func (s *RailsStore) lift() (Rails, error) {
	raw, exists, err := readFileNoFollow(s.statePath)
	if err != nil {
		return Rails{}, err
	}
	if !exists {
		return Rails{SchemaVersion: railsSchemaVersion}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Rails{}, fmt.Errorf("decode %s: %w", s.statePath, err)
	}
	doc := map[string]json.RawMessage{"schema_version": json.RawMessage("1")}
	for key := range liftedStateKeys {
		if value, ok := fields[key]; ok {
			doc[key] = value
		}
	}
	rawDoc, err := json.Marshal(doc)
	if err != nil {
		return Rails{}, fmt.Errorf("encode lifted %s: %w", s.statePath, err)
	}
	return decodeRails(s.statePath, rawDoc)
}

func decodeRails(path string, raw []byte) (Rails, error) {
	var rails Rails
	if err := json.Unmarshal(raw, &rails); err != nil {
		return Rails{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if rails.SchemaVersion != railsSchemaVersion {
		return Rails{}, fmt.Errorf("%s has schema_version %d, want %d",
			path, rails.SchemaVersion, railsSchemaVersion)
	}
	for i, move := range rails.RecentMoves {
		if move.SessionKey == "" || move.Account == "" || move.At.IsZero() {
			return Rails{}, fmt.Errorf("%s recent_moves[%d] is missing session_key, account, or at", path, i)
		}
	}
	for key := range rails.RefusedUntil {
		if key == "" {
			return Rails{}, fmt.Errorf("%s refused_until carries an empty session key", path)
		}
	}
	if rails.ReportsHeld != nil && rails.ReportsHeld.Since.IsZero() {
		return Rails{}, fmt.Errorf("%s reports_held carries no since timestamp", path)
	}
	for key, at := range rails.UnrecordedCarries {
		if key == "" || at.IsZero() {
			return Rails{}, fmt.Errorf("%s unrecorded_carries carries an empty path or timestamp", path)
		}
	}
	return rails, nil
}

// mutate applies fn under the sibling file lock and writes the result
// atomically — same posture as the state file: read inside the lock, fail
// closed on corrupt, preserve unknown fields. A mutation also CONVERGES
// the split: the rails file is written from the merged view, then the
// lifted keys are stripped from the state file so they cannot resurrect.
// The strip is best-effort — a failure leaves stale-but-inert keys behind
// (readers always prefer the rails file once it exists), it does not undo
// a commit that already landed.
func (s *RailsStore) mutate(fn func(*Rails) error) error {
	return config.WithFileLockTimeout(s.path, config.RepoInstancesLockTimeout, func() error {
		rails, extras, err := s.loadMerged()
		if err != nil {
			return err
		}
		if err := fn(&rails); err != nil {
			return err
		}
		raw, err := marshalPreserving(rails, extras, railsKeys, s.path)
		if err != nil {
			return err
		}
		if err := config.AtomicWriteFile(s.path, raw, 0o600); err != nil {
			return err
		}
		s.stripLiftedKeys()
		return nil
	})
}

// stripLiftedKeys removes the migrated mover keys from the state file. It
// runs inside the rails lock's callback but takes the state file's own
// lock — the only cross-lock direction in the pair (a state mutation never
// waits on the rails lock), so it cannot cycle. Best-effort by design: the
// keys it removes are inert the moment the rails file exists.
func (s *RailsStore) stripLiftedKeys() {
	_ = config.WithFileLockTimeout(s.statePath, config.RepoInstancesLockTimeout, func() error {
		raw, exists, err := readFileNoFollow(s.statePath)
		if err != nil || !exists {
			return nil
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil // corrupt state file — a reader's problem, not a strip's
		}
		dirty := false
		for key := range liftedStateKeys {
			if _, ok := fields[key]; ok {
				delete(fields, key)
				dirty = true
			}
		}
		if !dirty {
			return nil
		}
		out, err := json.MarshalIndent(fields, "", "  ")
		if err != nil {
			return nil
		}
		return config.AtomicWriteFile(s.statePath, out, 0o600)
	})
}

// RecordMove appends one committed automatic move. The live-mover slice is
// the writer; the switch-loop rail (counted per SessionKey) and the
// anti-pile-on credits (counted per target Agent + Account) read entries
// inside max_moves_window, so the record carries no verdict of its own.
func (s *RailsStore) RecordMove(sessionKey, agent, account string, at time.Time) error {
	if sessionKey == "" || agent == "" || account == "" || at.IsZero() {
		return fmt.Errorf("recent move needs a session key, an agent, an account, and a timestamp, got %q / %q / %q / %v",
			sessionKey, agent, account, at)
	}
	return s.mutate(func(rails *Rails) error {
		rails.RecentMoves = append(rails.RecentMoves,
			RecentMove{SessionKey: sessionKey, Agent: agent, Account: account, At: at})
		return nil
	})
}

// ClearMoves drops recent-move entries: just sessionKey's when it names one,
// every entry when it is empty. Reports how many cleared — a reset that
// matched nothing is still a truthful answer, never an error. It never
// materializes the file on an untouched home: absent IS the "no moves"
// answer.
func (s *RailsStore) ClearMoves(sessionKey string) (int, error) {
	rails, err := s.Load()
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, mv := range rails.RecentMoves {
		if sessionKey == "" || mv.SessionKey == sessionKey {
			pending++
		}
	}
	if pending == 0 {
		return 0, nil
	}
	cleared := 0
	if err := s.mutate(func(rails *Rails) error {
		kept := rails.RecentMoves[:0]
		for _, mv := range rails.RecentMoves {
			if sessionKey == "" || mv.SessionKey == sessionKey {
				cleared++
			} else {
				kept = append(kept, mv)
			}
		}
		if len(kept) == 0 {
			kept = nil
		}
		rails.RecentMoves = kept
		return nil
	}); err != nil {
		return 0, err
	}
	return cleared, nil
}

// SetRefusedUntil durably records that the session refused every candidate
// and is to be left alone until the given instant. The live-mover slice is
// the writer; an unexpired entry defers the session's next proposal.
func (s *RailsStore) SetRefusedUntil(sessionKey string, until time.Time) error {
	if sessionKey == "" || until.IsZero() {
		return fmt.Errorf("refusal backoff needs a session key and an until timestamp, got %q / %v",
			sessionKey, until)
	}
	return s.mutate(func(rails *Rails) error {
		if rails.RefusedUntil == nil {
			rails.RefusedUntil = map[string]time.Time{}
		}
		rails.RefusedUntil[sessionKey] = until
		return nil
	})
}

// ClearRefused drops refusal-backoff entries: just sessionKey's when it
// names one, every entry when it is empty. Reports how many cleared, and
// writes nothing when nothing matched — an untouched home keeps no file.
func (s *RailsStore) ClearRefused(sessionKey string) (int, error) {
	rails, err := s.Load()
	if err != nil {
		return 0, err
	}
	pending := 0
	if sessionKey == "" {
		pending = len(rails.RefusedUntil)
	} else if _, ok := rails.RefusedUntil[sessionKey]; ok {
		pending = 1
	}
	if pending == 0 {
		return 0, nil
	}
	cleared := 0
	if err := s.mutate(func(rails *Rails) error {
		if sessionKey == "" {
			cleared = len(rails.RefusedUntil)
			rails.RefusedUntil = nil
			return nil
		}
		if _, ok := rails.RefusedUntil[sessionKey]; ok {
			cleared = 1
			delete(rails.RefusedUntil, sessionKey)
		}
		if len(rails.RefusedUntil) == 0 {
			rails.RefusedUntil = nil
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return cleared, nil
}

// NoteReportsHeld durably records that report_to delivery has been held
// since the given instant. First-writer-wins: a second held backlog does
// not move the recorded since forward and erase the older hold.
func (s *RailsStore) NoteReportsHeld(since time.Time) error {
	if since.IsZero() {
		return fmt.Errorf("reports_held needs a since timestamp")
	}
	return s.mutate(func(rails *Rails) error {
		if rails.ReportsHeld == nil {
			rails.ReportsHeld = &ReportsHeld{Since: since}
		}
		return nil
	})
}

// ClearReportsHeld drops the held record when the last held backlog
// drains or dies — a cleared hold must not keep warning. It never
// materializes the file: absent IS the "nothing held" answer, and the
// daemon's startup reconcile runs on every home — writing a rails file
// onto an untouched one would opt its quota mover line in for a feature
// it never enabled (#6776, "absent table ≡ off ≡ today").
func (s *RailsStore) ClearReportsHeld() error {
	rails, err := s.Load()
	if err != nil {
		return err
	}
	if rails.ReportsHeld == nil {
		return nil
	}
	return s.mutate(func(rails *Rails) error {
		rails.ReportsHeld = nil
		return nil
	})
}

// mutateLiftTolerant is mutate with one relaxation, used ONLY by the
// unrecorded-carry marker: when the rails file does not exist yet and the
// load fails inside the lift — the STATE file is corrupt — the mutation
// runs on an empty document anyway. A corrupt state file holds no readable
// mover records, so nothing is dropped by starting empty; and the marker
// exists to outlive exactly this corruption, so refusing to write it would
// leave the carried rollout unquarantined forever. A corrupt RAILS file
// still fails closed like every other mutation.
func (s *RailsStore) mutateLiftTolerant(fn func(*Rails) error) error {
	return config.WithFileLockTimeout(s.path, config.RepoInstancesLockTimeout, func() error {
		rails, extras, err := s.loadMerged()
		if err != nil {
			_, exists, rerr := readFileNoFollow(s.path)
			if rerr != nil {
				return rerr
			}
			if exists {
				return err // the rails file itself is corrupt — fail closed
			}
			rails, extras = Rails{SchemaVersion: railsSchemaVersion}, nil
		}
		if err := fn(&rails); err != nil {
			return err
		}
		raw, err := marshalPreserving(rails, extras, railsKeys, s.path)
		if err != nil {
			return err
		}
		if err := config.AtomicWriteFile(s.path, raw, 0o600); err != nil {
			return err
		}
		s.stripLiftedKeys()
		return nil
	})
}

// NoteUnrecordedCarry records that a carry copied source-account bytes
// into path but its watermark could not be recorded — the file is
// quarantined end-to-end for retraction purposes until a real watermark
// lands. First-writer-wins on the timestamp. The marker tolerates a
// corrupt STATE file: it exists precisely for the home whose watermark
// file cannot be written, and that file holds no readable records to lose
// (mutateLiftTolerant). A corrupt rails file still fails closed.
//
// At the entry bound an eviction is required; it is reported (never
// silent — evicting re-trusts a file) and chosen fail-safe-first: a
// marker whose file no longer exists protects nothing, so gone files
// retire before the oldest live one.
func (s *RailsStore) NoteUnrecordedCarry(path string, at time.Time) (evicted string, err error) {
	if path == "" || at.IsZero() {
		return "", fmt.Errorf("unrecorded carry needs a path and a timestamp, got %q / %v", path, at)
	}
	err = s.mutateLiftTolerant(func(rails *Rails) error {
		if rails.UnrecordedCarries == nil {
			rails.UnrecordedCarries = map[string]time.Time{}
		}
		if _, ok := rails.UnrecordedCarries[path]; !ok {
			rails.UnrecordedCarries[path] = at
			if len(rails.UnrecordedCarries) > unrecordedCarryCap {
				evicted = evictUnrecordedCarry(rails.UnrecordedCarries, path)
				delete(rails.UnrecordedCarries, evicted)
			}
		}
		return nil
	})
	return evicted, err
}

// evictUnrecordedCarry picks the marker to retire: a path whose file is
// gone (quarantining nothing), else the oldest entry — the residual
// exposure lands on the stalest record, never the carry that just landed
// (Codex review on the cap).
func evictUnrecordedCarry(entries map[string]time.Time, keep string) string {
	var oldest string
	var oldestAt time.Time
	for path, at := range entries {
		if path == keep {
			continue
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path
		}
		if oldest == "" || at.Before(oldestAt) {
			oldest, oldestAt = path, at
		}
	}
	return oldest
}

// ClearUnrecordedCarry retires one quarantine marker — a recorded
// watermark now bounds the file's poison precisely, so the blanket
// distrust lifts. No write when there is nothing to retire.
func (s *RailsStore) ClearUnrecordedCarry(path string) error {
	rails, err := s.Load()
	if err != nil {
		return err
	}
	if _, ok := rails.UnrecordedCarries[path]; !ok {
		return nil
	}
	return s.mutate(func(rails *Rails) error {
		delete(rails.UnrecordedCarries, path)
		if len(rails.UnrecordedCarries) == 0 {
			rails.UnrecordedCarries = nil
		}
		return nil
	})
}

// PruneExpired drops records that can no longer matter: refusal backoffs
// whose instant has passed, and recent-move entries outside window.
// max_moves_window has ONE meaning everywhere it is read — records older
// than the window do not count — so the same bound trims the file the
// counters no longer consult (#6776 review). window <= 0 is unbounded:
// every record still counts, so nothing is pruned. Never materializes the
// file when there is nothing to prune.
func (s *RailsStore) PruneExpired(window time.Duration, now time.Time) error {
	rails, err := s.Load()
	if err != nil {
		return err
	}
	needs := false
	for _, until := range rails.RefusedUntil {
		if !until.After(now) {
			needs = true
			break
		}
	}
	if window > 0 {
		cutoff := now.Add(-window)
		for _, mv := range rails.RecentMoves {
			if mv.At.Before(cutoff) {
				needs = true
				break
			}
		}
	}
	if !needs {
		return nil
	}
	return s.mutate(func(rails *Rails) error {
		for key, until := range rails.RefusedUntil {
			if !until.After(now) {
				delete(rails.RefusedUntil, key)
			}
		}
		if len(rails.RefusedUntil) == 0 {
			rails.RefusedUntil = nil
		}
		if window > 0 {
			cutoff := now.Add(-window)
			kept := rails.RecentMoves[:0]
			for _, mv := range rails.RecentMoves {
				if !mv.At.Before(cutoff) {
					kept = append(kept, mv)
				}
			}
			if len(kept) == 0 {
				kept = nil
			}
			rails.RecentMoves = kept
		}
		return nil
	})
}
