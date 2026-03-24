package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/sachiniyer/agent-factory/log"
	"os"
	"path/filepath"
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
	GetInstances(repoID string) json.RawMessage
	// GetAllInstances returns instance data for all repos, keyed by repo ID.
	GetAllInstances() map[string]json.RawMessage
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

// StateManager combines instance storage and app state management
type StateManager interface {
	InstanceStorage
	AppState
}

// State represents the application state that persists between sessions
type State struct {
	// HelpScreensSeen is a bitmask tracking which help screens have been shown
	HelpScreensSeen uint32 `json:"help_screens_seen"`
}

// DefaultState returns the default state
func DefaultState() *State {
	return &State{
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

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		log.ErrorLog.Printf("failed to parse state file: %v", err)
		return DefaultState()
	}

	return &state
}

// SaveState saves the state to disk
func SaveState(state *State) error {
	configDir, err := GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}

	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	statePath := filepath.Join(configDir, StateFileName)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	return AtomicWriteFile(statePath, data, 0644)
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
	dir, err := instancesDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, repoID, InstancesFileName), nil
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
	return json.RawMessage(data), nil
}

// SaveRepoInstances saves instances for a specific repo using atomic writes.
func SaveRepoInstances(repoID string, data json.RawMessage) error {
	path, err := repoInstancesPath(repoID)
	if err != nil {
		return err
	}
	return AtomicWriteFile(path, data, 0644)
}

// RepoInstancesPath returns the file path for a repo's instances file.
// Exported so callers can use it for file locking.
func RepoInstancesPath(repoID string) (string, error) {
	return repoInstancesPath(repoID)
}

// UpdateRepoInstances loads instances under a file lock, applies fn, and saves atomically.
func UpdateRepoInstances(repoID string, fn func(raw json.RawMessage) (json.RawMessage, error)) error {
	path, err := repoInstancesPath(repoID)
	if err != nil {
		return err
	}
	return WithFileLock(path, func() error {
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
		data, err := LoadRepoInstances(repoID)
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

func (s *State) GetInstances(repoID string) json.RawMessage {
	data, err := LoadRepoInstances(repoID)
	if err != nil {
		log.ErrorLog.Printf("failed to load repo instances: %v", err)
		return json.RawMessage("[]")
	}
	return data
}

func (s *State) GetAllInstances() map[string]json.RawMessage {
	data, err := LoadAllRepoInstances()
	if err != nil {
		log.ErrorLog.Printf("failed to load all repo instances: %v", err)
		return make(map[string]json.RawMessage)
	}
	return data
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
