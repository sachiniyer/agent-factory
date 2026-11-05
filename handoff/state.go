// Package handoff owns the durable record shared by the native
// account-handoff slices (#6776): <af-home>/instances/auto-handoff-state.json.
//
// The file carries state that must survive a daemon restart — today the
// per-file carried-prefix watermarks the usage reader honors, and the halt
// record af quota surfaces; the rails slices extend it additively
// (recent-move credits, refusal backoff, ambient wall bookkeeping). It is
// written under a sibling file lock with an atomic rename and FAILS CLOSED on
// decode: a corrupt rails file must stop the mover, never be silently rebuilt
// — the same posture readAccountLimitLedger takes.
package handoff

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// StateFileName is the file's name under <af-home>/instances/.
const StateFileName = "auto-handoff-state.json"

const stateSchemaVersion = 1

// State is the durable auto-handoff record. Fields are additive: readers
// tolerate fields a newer slice writes, and every field must be meaningful
// when absent (omitempty everywhere) so a file written by an older af still
// decodes.
type State struct {
	SchemaVersion int `json:"schema_version"`
	// CarriedPrefixes maps a rollout file's absolute path to the number of
	// leading bytes a same-agent account carry copied from the SOURCE
	// account's file. Records inside that prefix hold the source account's
	// rate-limit numbers, so the usage reader ignores claims whose line
	// begins below the watermark; records the target's own runtime appended
	// past it count normally. Keyed per file rather than per account because
	// an account-wide carried_at would blind genuine readings from other
	// sessions (design R3).
	CarriedPrefixes map[string]int64 `json:"carried_prefix_len,omitempty"`
	// Halted records a mover halt (on_failure = "halt"). Presence means
	// engine-originated moves are stopped; ordinary wait-for-reset resume is
	// unaffected. Written by the rails slice; read by `af quota` and the
	// status surface so a halted mover is never silent (design R16).
	Halted *Halt `json:"halted,omitempty"`
}

// Halt is the durable halt record: when the mover stopped and why.
type Halt struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
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
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{SchemaVersion: stateSchemaVersion}, nil
		}
		return State{}, fmt.Errorf("read %s: %w", s.path, err)
	}
	return decodeState(s.path, raw)
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
	return state, nil
}

// mutate applies fn under the sibling file lock and writes the result
// atomically. The read happens INSIDE the lock so a racing mutator cannot be
// clobbered; a corrupt file fails fn's caller rather than being rebuilt on
// top of.
func (s *Store) mutate(fn func(*State) error) error {
	return config.WithFileLockTimeout(s.path, config.RepoInstancesLockTimeout, func() error {
		state, err := s.Load()
		if err != nil {
			return err
		}
		if err := fn(&state); err != nil {
			return err
		}
		raw, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", s.path, err)
		}
		return config.AtomicWriteFile(s.path, raw, 0o600)
	})
}

// RecordCarriedPrefix durably records that the first n bytes of the rollout
// at path were copied from the source account's file by a carry. n must be a
// record boundary — the reader never parses a half-line.
func (s *Store) RecordCarriedPrefix(path string, n int64) error {
	if path == "" || n < 0 {
		return fmt.Errorf("carried prefix needs a path and a non-negative length, got %q / %d", path, n)
	}
	return s.mutate(func(state *State) error {
		if state.CarriedPrefixes == nil {
			state.CarriedPrefixes = map[string]int64{}
		}
		state.CarriedPrefixes[path] = n
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
