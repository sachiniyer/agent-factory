package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

const (
	StateFileName     = "state.json"
	InstancesFileName = "instances.json"
)

// InstanceStorage handles instance-related operations with per-repo scoping.
type InstanceStorage interface {
	// SaveInstances saves the raw instance data for a specific repo.
	SaveInstances(repoID string, instancesJSON json.RawMessage) error
	// GetInstances returns the raw instance data for a specific repo.
	//
	// A missing instances file yields "[]" with a nil error so first-run
	// callers proceed normally. Any other failure (permission denied,
	// I/O/NFS error, truncation) is PROPAGATED rather than masked as an
	// empty list — read-modify-write callers must not merge against, and
	// then overwrite, disk state they failed to read (#766).
	GetInstances(repoID string) (json.RawMessage, error)
	// GetAllInstances returns instance data for all repos, keyed by repo ID.
	//
	// A missing instances directory yields an empty map with a nil error so
	// first-run callers proceed normally. A genuine failure to read the
	// instances directory (permission denied, I/O error) is PROPAGATED rather
	// than masked as an empty map: callers like `af reset` and the daemon must
	// distinguish "no sessions exist" from "sessions exist but are unreadable"
	// so they don't skip worktree cleanup or silently hide live sessions
	// (#868). This mirrors GetInstances' single-repo error propagation (#766).
	// Per-repo files that individually fail to read are skipped-and-warned by
	// LoadAllRepoInstances, so they never surface as an error here.
	GetAllInstances() (map[string]json.RawMessage, error)
	// DeleteAllInstances removes all stored instances across all repos.
	DeleteAllInstances() error
}

// AppState handles application-level state
type AppState interface {
	// GetHelpScreensSeen returns the bitmask of seen help screens
	GetHelpScreensSeen() uint32
	// SetHelpScreensSeen updates the bitmask of seen help screens
	SetHelpScreensSeen(seen uint32) error
}

// State represents the application state that persists between sessions
type State struct {
	SchemaVersion int `json:"schema_version"`
	// HelpScreensSeen is a bitmask tracking which help screens have been shown
	HelpScreensSeen uint32 `json:"help_screens_seen"`
	// OnboardingSeen records that the user has been offered the config-agent
	// walkthrough — whether they took it, skipped it, or it failed to start. It
	// is what stops af asking again on every launch.
	//
	// A MARKER rather than a config-file check, because the obvious test is not
	// merely fragile but always false: LoadConfig MATERIALIZES config.toml as a
	// side effect (config_load.go, materializeDefaultConfig) and the TUI calls it
	// during boot, so "no config.toml yet" has already stopped being true by the
	// time anything could ask. "Config looks default" is no better —
	// DefaultConfig() shells out to detect the claude command, so its content
	// varies by machine. Positive evidence is the only thing that survives.
	//
	// Additive: a new field needs no schema bump (an older af ignores it, a newer
	// one defaults it to false and re-offers once).
	OnboardingSeen bool `json:"onboarding_seen"`
}

// DefaultState returns the default state
func DefaultState() *State {
	return &State{
		SchemaVersion:   StateSchemaVersion,
		HelpScreensSeen: 0,
	}
}

// LoadState loads the state from disk. If it cannot be done, we return the default state.
func LoadState() *State {
	configDir, err := GetConfigDir()
	if err != nil {
		log.ErrorLog.Printf("failed to get config directory: %v", err)
		return DefaultState()
	}

	statePath := filepath.Join(configDir, StateFileName)
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create and save default state if file doesn't exist
			defaultState := DefaultState()
			if saveErr := SaveState(defaultState); saveErr != nil {
				log.WarningLog.Printf("failed to save default state: %v", saveErr)
			}
			return defaultState
		}

		log.WarningLog.Printf("failed to get state file: %v", err)
		return DefaultState()
	}

	if version, err := DetectJSONSchemaVersion(data); err != nil {
		log.ErrorLog.Printf("failed to detect state file schema version: %v", err)
		return DefaultState()
	} else if version > StateSchemaVersion {
		log.ErrorLog.Printf("state file has schema_version %d, but this binary supports up to %d; using defaults", version, StateSchemaVersion)
		return DefaultState()
	}

	state := DefaultState()
	if err := json.Unmarshal(data, state); err != nil {
		log.ErrorLog.Printf("failed to parse state file: %v", err)
		return DefaultState()
	}
	if state.SchemaVersion == LegacySchemaVersion {
		state.SchemaVersion = StateSchemaVersion
	}

	return state
}

// SaveState saves the state to disk.
//
// The write is refused when the on-disk state.json carries a schema_version
// newer than this binary understands: an older af must never clobber (and thus
// downgrade/corrupt) state written by a newer af. This mirrors the save-blocking
// guard config.toml and tui-state.json already enforce (#1725). Same- or
// older-schema saves proceed unchanged; a missing or unreadable/corrupt file
// does not block the write.
func SaveState(state *State) error {
	configDir, err := GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}

	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	statePath := filepath.Join(configDir, StateFileName)
	return WithFileLock(statePath, func() error {
		if err := refuseStateDowngrade(statePath); err != nil {
			return err
		}
		state.SchemaVersion = StateSchemaVersion
		data, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal state: %w", err)
		}
		return AtomicWriteFile(statePath, data, 0644)
	})
}

// TrySaveState is SaveState without the wait: it takes the state lock with
// TryWithFileLock and reports acquired=false rather than blocking when another
// process holds it.
//
// It exists for the LAUNCH CRITICAL PATH. SaveState takes a blocking
// WithFileLock with no timeout, and this repo has a documented bug class where
// work moved in front of the TUI turns a benign lock into a launch hang — af
// starting up while another af (or a daemon) holds state.json would simply never
// draw. A marker write is not worth that risk: the cost of losing the lock race
// is that onboarding is offered once more on the next launch, which is a far
// better failure than a terminal that never comes back.
//
// Callers on the launch path must treat acquired=false as "skip this write",
// never as an error to surface or retry.
func TrySaveState(state *State) (bool, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return false, fmt.Errorf("failed to get config directory: %w", err)
	}
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return false, fmt.Errorf("failed to create config directory: %w", err)
	}

	statePath := filepath.Join(configDir, StateFileName)
	return TryWithFileLock(statePath, func() error {
		if err := refuseStateDowngrade(statePath); err != nil {
			return err
		}
		state.SchemaVersion = StateSchemaVersion
		data, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal state: %w", err)
		}
		return AtomicWriteFile(statePath, data, 0644)
	})
}

// refuseStateDowngrade blocks a save that would downgrade/clobber a state.json
// this binary must not overwrite. It fails closed on ambiguity: the write is
// only allowed when the on-disk schema is definitively same-or-older (or
// absent/legacy). A missing file (first run) or a structurally-corrupt file
// recovers to a freshly-written default rather than wedging saves forever, but a
// schema_version that is PRESENT yet cannot be parsed/interpreted as a version
// this binary understands (non-integer, out of range, wrong type) is treated as
// "possibly newer" and refused — a value we cannot read might come from a newer
// af, so we must not clobber it (#1725).
func refuseStateDowngrade(statePath string) error {
	data, err := os.ReadFile(statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.WarningLog.Printf("could not read %s before saving state; overwriting: %v", statePath, err)
		}
		return nil
	}

	present, version, verr := onDiskStateSchemaVersion(data)
	switch {
	case !present:
		// Absent field, legacy array-root, or a structurally-corrupt file: none
		// of these is a newer schema. Legacy files upgrade in place; corrupt
		// files recover to a default on write.
		if verr != nil {
			log.WarningLog.Printf("could not detect schema version of %s before saving state; overwriting: %v", statePath, verr)
		}
		return nil
	case verr != nil:
		// schema_version is present but uninterpretable — fail closed.
		return fmt.Errorf("%s has an unrecognizable schema_version (%v); this binary supports up to %d and will not overwrite it, as it may have been written by a newer af — upgrade af",
			describeSchemaStore(StateFileName, statePath), verr, StateSchemaVersion)
	case version > StateSchemaVersion:
		return &UnsupportedSchemaVersionError{
			StoreName:        StateFileName,
			Path:             statePath,
			FileVersion:      version,
			SupportedVersion: StateSchemaVersion,
		}
	default:
		return nil
	}
}

// onDiskStateSchemaVersion reports whether a state.json blob carries a
// schema_version field and, if so, how it parses. present is false for a
// structurally-corrupt blob (not a JSON object), a JSON array (legacy), or an
// object lacking the field — with verr carrying the parse failure for the
// corrupt case. present is true when the field exists; verr is then non-nil iff
// the value could not be interpreted as a version integer.
func onDiskStateSchemaVersion(data []byte) (present bool, version int, verr error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return false, LegacySchemaVersion, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return false, LegacySchemaVersion, fmt.Errorf("trailing data after JSON value")
	}
	obj, ok := root.(map[string]any)
	if !ok {
		// Arrays are legacy v0; any other scalar root is corrupt. Neither is newer.
		if _, isArray := root.([]any); isArray {
			return false, LegacySchemaVersion, nil
		}
		return false, LegacySchemaVersion, fmt.Errorf("JSON root must be an object, got %T", root)
	}
	value, ok := obj[SchemaVersionField]
	if !ok {
		return false, LegacySchemaVersion, nil
	}
	version, verr = schemaVersionFromValue(value)
	return true, version, verr
}

// Per-repo instance file functions

func instancesDirPath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "instances"), nil
}

func repoInstancesPath(repoID string) (string, error) {
	if err := ValidateRepoID(repoID); err != nil {
		return "", err
	}
	dir, err := instancesDirPath()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, repoID, InstancesFileName)
	// Defense in depth: confirm the joined path remains inside the
	// instances directory. ValidateRepoID already rejects "..", "/",
	// and "\", so this is a belt-and-suspenders check in case the
	// regex is ever loosened.
	cleanDir := filepath.Clean(dir) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(path), cleanDir) {
		return "", fmt.Errorf("invalid repo id: resolved path escapes instances directory")
	}
	return path, nil
}

// LoadRepoInstances loads instances for a specific repo.
func LoadRepoInstances(repoID string) (json.RawMessage, error) {
	path, err := repoInstancesPath(repoID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return json.RawMessage("[]"), nil
		}
		return nil, fmt.Errorf("failed to read repo instances: %w", err)
	}
	// Handle empty file (e.g. from interrupted write)
	if len(bytes.TrimSpace(data)) == 0 {
		return json.RawMessage("[]"), nil
	}
	return extractInstancesArray(data, path)
}

// SaveRepoInstances saves instances for a specific repo using atomic writes.
func SaveRepoInstances(repoID string, data json.RawMessage) error {
	path, err := repoInstancesPath(repoID)
	if err != nil {
		return err
	}
	diskData, err := marshalInstancesEnvelope(data)
	if err != nil {
		// Several recovery tests deliberately plant corrupt bytes through this
		// low-level helper. Preserve that ability while normal valid array saves
		// move to the v1 envelope.
		if !json.Valid(data) {
			diskData = data
		} else {
			return err
		}
	}
	return AtomicWriteFile(path, diskData, 0644)
}

// RepoInstancesPath returns the file path for a repo's instances file.
// Exported so callers can use it for file locking.
func RepoInstancesPath(repoID string) (string, error) {
	return repoInstancesPath(repoID)
}

// RepoInstancesLockTimeout bounds how long UpdateRepoInstances waits for a repo's
// instances flock. A var so tests can shorten it; production never reassigns.
//
// This lock is held only across a read-modify-write of one small JSON file, so
// waiting seconds for it already means a peer is wedged rather than busy — and
// the daemon holds much bigger things while it waits. A kill holds its session's
// kill guard and op lock across this write, and the Lost-restore loop holds the
// same op lock across its own; an unbounded wait there does not stall one write,
// it makes the session permanently undeletable and starves the tombstone finisher
// that would otherwise heal it (#1917). Failing with a retryable error is always
// better than a daemon that cannot be asked to stop.
var RepoInstancesLockTimeout = 10 * time.Second

// UpdateRepoInstances loads instances under a file lock, applies fn, and saves
// atomically. The lock is taken with a DEADLINE (see RepoInstancesLockTimeout):
// contention surfaces as a retryable ErrLockTimeout rather than parking the
// caller — and every caller here is the daemon, holding session-wide locks.
func UpdateRepoInstances(repoID string, fn func(raw json.RawMessage) (json.RawMessage, error)) error {
	path, err := repoInstancesPath(repoID)
	if err != nil {
		return err
	}
	return WithFileLockTimeout(path, RepoInstancesLockTimeout, func() error {
		raw, err := LoadRepoInstances(repoID)
		if err != nil {
			return err
		}
		result, err := fn(raw)
		if err != nil {
			return err
		}
		return SaveRepoInstances(repoID, result)
	})
}

// DeleteRepoInstances deletes instances for a specific repo.
func DeleteRepoInstances(repoID string) error {
	path, err := repoInstancesPath(repoID)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LoadAllRepoInstances loads instances from all per-repo files.
func LoadAllRepoInstances() (map[string]json.RawMessage, error) {
	dir, err := instancesDirPath()
	if err != nil {
		return nil, err
	}
	result := make(map[string]json.RawMessage)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, fmt.Errorf("failed to read instances directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoID := entry.Name()
		data, err := loadRepoInstancesForAll(repoID)
		if err != nil {
			log.WarningLog.Printf("failed to load instances for repo %s: %v", repoID, err)
			continue
		}
		result[repoID] = data
	}
	return result, nil
}

// DeleteAllRepoInstances deletes all per-repo instance files.
func DeleteAllRepoInstances() error {
	dir, err := instancesDirPath()
	if err != nil {
		return err
	}
	err = os.RemoveAll(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// InstanceStorage interface implementation

func (s *State) SaveInstances(repoID string, instancesJSON json.RawMessage) error {
	return SaveRepoInstances(repoID, instancesJSON)
}

func (s *State) GetInstances(repoID string) (json.RawMessage, error) {
	// LoadRepoInstances already distinguishes "missing file" (-> "[]", nil)
	// from a genuine read error, so surface its result verbatim. Swallowing the
	// error here would let a read-modify-write writer (the daemon's targeted RMW
	// primitives) merge against an empty disk state and clobber
	// unreadable-but-present sessions (#766).
	return LoadRepoInstances(repoID)
}

func (s *State) GetAllInstances() (map[string]json.RawMessage, error) {
	// LoadAllRepoInstances already distinguishes "first run" (instances dir
	// missing -> empty map, nil) from a genuine directory read error, and
	// skips-and-warns per-repo files it cannot read. Surface its result
	// verbatim: swallowing the error as an empty map is what let reset skip
	// worktree cleanup and the daemon hide unreadable-but-present sessions
	// (#868).
	return LoadAllRepoInstances()
}

func (s *State) DeleteAllInstances() error {
	return DeleteAllRepoInstances()
}

// AppState interface implementation

// GetHelpScreensSeen returns the bitmask of seen help screens
func (s *State) GetHelpScreensSeen() uint32 {
	return s.HelpScreensSeen
}

// SetHelpScreensSeen updates the bitmask of seen help screens
func (s *State) SetHelpScreensSeen(seen uint32) error {
	s.HelpScreensSeen = seen
	return SaveState(s)
}
