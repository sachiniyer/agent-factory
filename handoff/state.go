// Package handoff owns the durable records shared by the native
// account-handoff slices (#6776). Two files live under
// <af-home>/instances/:
//
//   - auto-handoff-state.json — the file v1.0.345 already reads and writes:
//     the per-file carried-prefix watermarks the usage reader honors, and
//     the halt record af quota surfaces. Its schema stays exactly the field
//     set v1.0.345 models, so a downgrade's read-modify-write cannot erase
//     anything here.
//   - auto-handoff-rails.json — everything v1.0.345 does NOT model (move
//     credits, refusal backoffs, held-report records, unrecorded-carry
//     quarantine). v1.0.345 rewrites the state file on every carry and
//     drops fields it does not know, so these records MUST live in a file
//     it never opens. See rails.go.
//
// Both files are written under a sibling file lock with an atomic rename
// and FAIL CLOSED on decode: a corrupt file must stop the mover, never be
// silently rebuilt — the same posture readAccountLimitLedger takes. Reads
// open O_NOFOLLOW (#3672): a symlinked rails or watermark file is a
// tampering signal, not something to follow. And every read-modify-write
// preserves top-level fields the writing binary does not model, so a later
// slice's fields survive an older writer that knows this rule.
package handoff

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// StateFileName is the watermark/halt file's name under <af-home>/instances/.
const StateFileName = "auto-handoff-state.json"

const stateSchemaVersion = 1

// State is the durable record v1.0.345 already models — carried-prefix
// watermarks plus the mover halt — and NOTHING else: fields a newer slice
// adds live in the rails file (rails.go), because v1.0.345's rewrite drops
// any field it does not know. Reads tolerate unknown fields; every modeled
// field is meaningful when absent (omitempty everywhere) so a file written
// by an older af still decodes.
type State struct {
	SchemaVersion int `json:"schema_version"`
	// CarriedPrefixes maps a rollout file's absolute path to the number of
	// leading bytes a same-agent account carry copied from the SOURCE
	// account's file. Records inside that prefix hold the source account's
	// rate-limit numbers, so the usage reader ignores claims whose line
	// begins below the watermark; records the target's own runtime appended
	// past it count normally. Keyed per file rather than per account because
	// an account-wide carried_at would blind genuine readings from other
	// sessions (design R3). An entry of 0 is a record too: a carry af
	// committed there copied no bytes. Only an absent entry means af never
	// recorded a carry for the file (Snapshot.CarriedBoundary, #7270); a
	// binary that predates the distinction reads a 0 exactly as absent. A
	// record, zero or not, is complete only while every binary that writes
	// the path records its carries: one older than v1.0.345 (Codex, #6802)
	// or v1.0.349 (Claude, #6975) carries without a watermark, so a
	// downgrade past those can leave a record that understates (#7277).
	CarriedPrefixes map[string]int64 `json:"carried_prefix_len,omitempty"`
	// Halted records a mover halt (on_failure = "halt"). Presence means
	// engine-originated moves are stopped; ordinary wait-for-reset resume is
	// unaffected. Written by the rails slice; read by `af quota` and the
	// status surface so a halted mover is never silent (design R16). It
	// lives in the SHARED file because v1.0.345 models it — a halt recorded
	// here still blocks a downgraded binary's swaps.
	Halted *Halt `json:"halted,omitempty"`
}

// Halt is the durable halt record: when the mover stopped and why.
type Halt struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// stateKeys are the top-level fields this binary models in the state file;
// a read-modify-write preserves every other key verbatim (marshalPreserving
// in store_io.go).
var stateKeys = map[string]bool{
	"schema_version":     true,
	"carried_prefix_len": true,
	"halted":             true,
}

// Store reads and mutates one auto-handoff state file.
type Store struct {
	path string
}

// DefaultPath resolves <af-home>/instances/auto-handoff-state.json.
func DefaultPath() (string, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "instances", StateFileName), nil
}

// DefaultStore opens the store at DefaultPath.
func DefaultStore() (*Store, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return &Store{path: path}, nil
}

// NewStore opens the store at an explicit path (tests, non-default homes).
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path is the file this store reads and writes.
func (s *Store) Path() string {
	return s.path
}

// Load reads the state file. A missing file is an empty state — absent is a
// real answer ("nothing recorded"), unlike a CORRUPT file, which errors:
// callers deciding on corrupt rails state must fail closed.
func (s *Store) Load() (State, error) {
	state, _, err := s.loadDoc()
	return state, err
}

// loadDoc returns the decoded state plus the file's raw top-level fields —
// the pair a preserving rewrite needs.
func (s *Store) loadDoc() (State, map[string]json.RawMessage, error) {
	raw, exists, err := readFileNoFollow(s.path)
	if err != nil {
		return State{}, nil, err
	}
	if !exists {
		return State{SchemaVersion: stateSchemaVersion}, nil, nil
	}
	state, err := decodeState(s.path, raw)
	if err != nil {
		return State{}, nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return State{}, nil, fmt.Errorf("decode %s: %w", s.path, err)
	}
	return state, fields, nil
}

func decodeState(path string, raw []byte) (State, error) {
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if state.SchemaVersion != stateSchemaVersion {
		return State{}, fmt.Errorf("%s has schema_version %d, want %d",
			path, state.SchemaVersion, stateSchemaVersion)
	}
	for file, prefix := range state.CarriedPrefixes {
		if prefix < 0 {
			return State{}, fmt.Errorf("%s carries a negative prefix length for %s", path, file)
		}
	}
	if state.Halted != nil && (state.Halted.At.IsZero() || state.Halted.Reason == "") {
		return State{}, fmt.Errorf("%s halted is missing at or reason", path)
	}
	return state, nil
}

// mutate applies fn under the sibling file lock and writes the result
// atomically. The read happens INSIDE the lock so a racing mutator cannot be
// clobbered; a corrupt file fails fn's caller rather than being rebuilt on
// top of. Fields this binary does not model survive the rewrite
// (marshalPreserving in store_io.go).
func (s *Store) mutate(fn func(*State) error) error {
	return config.WithFileLockTimeout(s.path, config.RepoInstancesLockTimeout, func() error {
		state, extras, err := s.loadDoc()
		if err != nil {
			return err
		}
		if err := fn(&state); err != nil {
			return err
		}
		raw, err := marshalPreserving(state, extras, stateKeys, s.path)
		if err != nil {
			return err
		}
		return config.AtomicWriteFile(s.path, raw, 0o600)
	})
}

// RecordCarriedPrefix durably records that the first n bytes of the rollout
// at path were copied from the source account's file by a carry. n must be a
// record boundary — the reader never parses a half-line. The watermark only
// ever grows: a re-carry that verifies a shorter covered prefix must not
// shrink the quarantine onto bytes an earlier, longer copy put there —
// records in that span are still the source account's (#6802 review). n may
// be 0: a carry that copied nothing still records that it ran, so a later
// re-run can tell "carried zero bytes" from "never recorded" (#7270).
func (s *Store) RecordCarriedPrefix(path string, n int64) error {
	if path == "" || n < 0 {
		return fmt.Errorf("carried prefix needs a path and a non-negative length, got %q / %d", path, n)
	}
	return s.mutate(func(state *State) error {
		if state.CarriedPrefixes == nil {
			state.CarriedPrefixes = map[string]int64{}
		}
		if existing, ok := state.CarriedPrefixes[path]; !ok || n > existing {
			state.CarriedPrefixes[path] = n
		}
		return nil
	})
}

// CarriedPrefixes loads the watermark map, or nil when none is recorded.
func (s *Store) CarriedPrefixes() (map[string]int64, error) {
	state, err := s.Load()
	if err != nil {
		return nil, err
	}
	return state.CarriedPrefixes, nil
}

// HaltedAt loads the halt record, or nil when the mover is not halted.
func (s *Store) HaltedAt() (*Halt, error) {
	state, err := s.Load()
	if err != nil {
		return nil, err
	}
	return state.Halted, nil
}

// RecordHalt writes the durable mover halt (on_failure = "halt"): from this
// instant engine-originated moves are stopped until an operator clears the
// record. The live-mover slice is the writer; the reset surface and af quota
// are the readers. A halt carries a reason — a reasonless halt would stop
// the mover silently, which is the failure R16 exists to prevent.
func (s *Store) RecordHalt(reason string, at time.Time) error {
	if reason == "" || at.IsZero() {
		return fmt.Errorf("halt needs a reason and a timestamp, got %q / %v", reason, at)
	}
	return s.mutate(func(state *State) error {
		state.Halted = &Halt{At: at, Reason: reason}
		return nil
	})
}

// ClearHalt drops the halt record, returning the cleared record — nil when
// no halt stood — so a reset can say what it removed rather than claiming a
// clearance it did not perform. It never materializes the file: absent IS
// the "no halt" answer, so a reset on an untouched home writes nothing.
func (s *Store) ClearHalt() (*Halt, error) {
	state, err := s.Load()
	if err != nil {
		return nil, err
	}
	if state.Halted == nil {
		return nil, nil
	}
	var cleared *Halt
	if err := s.mutate(func(state *State) error {
		cleared = state.Halted
		state.Halted = nil
		return nil
	}); err != nil {
		return nil, err
	}
	return cleared, nil
}
