package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// TUIStateFile is the global, TUI-owned view-state envelope. It is keyed by
// repo ID because one user-level TUI state file covers every local repo.
type TUIStateFile struct {
	SchemaVersion int                         `json:"schema_version"`
	Repos         map[string]TUIRepoViewState `json:"repos"`
}

// TUIRepoViewState is the client-side projection state restored by the TUI.
type TUIRepoViewState struct {
	UpdatedAt time.Time          `json:"updated_at,omitempty"`
	Selected  *TUIStateTarget    `json:"selected,omitempty"`
	ActiveTab *TUIStateTarget    `json:"active_tab,omitempty"`
	Focus     *TUIStateFocus     `json:"focus,omitempty"`
	OpenPanes []TUIStateOpenPane `json:"open_panes,omitempty"`
}

// TUIStateTarget identifies a session/tab by stable identity, falling back to
// title for older records that did not carry a session ID.
type TUIStateTarget struct {
	InstanceID string `json:"instance_id,omitempty"`
	Title      string `json:"title,omitempty"`
	TabName    string `json:"tab_name,omitempty"`
}

// TUIStateFocus records the focused TUI region. Pane focus uses PaneKey instead
// of an in-process pane ID, since pane IDs are reallocated on restore.
type TUIStateFocus struct {
	Region  string `json:"region"`
	PaneKey string `json:"pane_key,omitempty"`
}

// TUIStateOpenPane records one explicit workspace pane binding.
type TUIStateOpenPane struct {
	Key        string `json:"key"`
	InstanceID string `json:"instance_id,omitempty"`
	Title      string `json:"title,omitempty"`
	TabName    string `json:"tab_name"`
	FocusRank  uint64 `json:"focus_rank,omitempty"`
}

// LoadTUIRepoViewState loads the saved TUI state for repoID. Missing, corrupt,
// legacy, or newer files return defaults and log a warning instead of crashing
// the TUI; the bool reports whether a repo entry was present and valid.
func LoadTUIRepoViewState(repoID string) (TUIRepoViewState, bool) {
	if err := ValidateRepoID(repoID); err != nil {
		log.WarningLog.Printf("failed to load TUI state: %v", err)
		return TUIRepoViewState{}, false
	}
	path, err := TUIStatePath()
	if err != nil {
		log.WarningLog.Printf("failed to resolve TUI state path: %v", err)
		return TUIRepoViewState{}, false
	}
	file, ok, err := readTUIStateFile(path)
	if err != nil {
		log.WarningLog.Printf("failed to load TUI state from %s: %v", path, err)
		return TUIRepoViewState{}, false
	}
	if !ok || file.Repos == nil {
		return TUIRepoViewState{}, false
	}
	state, ok := file.Repos[repoID]
	return state, ok
}

// SaveTUIRepoViewState updates only repoID's entry under the global TUI state
// file lock, preserving other repos' client view-state.
func SaveTUIRepoViewState(repoID string, state TUIRepoViewState) error {
	if err := ValidateRepoID(repoID); err != nil {
		return err
	}
	path, err := TUIStatePath()
	if err != nil {
		return err
	}
	return WithFileLock(path, func() error {
		file, err := loadTUIStateFileForUpdate(path)
		if err != nil {
			return err
		}
		if file.Repos == nil {
			file.Repos = make(map[string]TUIRepoViewState)
		}
		file.SchemaVersion = TUIStateSchemaVersion
		file.Repos[repoID] = state
		return writeTUIStateFile(path, file)
	})
}

func readTUIStateFile(path string) (TUIStateFile, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultTUIStateFile(), false, nil
		}
		return TUIStateFile{}, false, err
	}
	file, err := decodeTUIStateFile(raw, path)
	if err != nil {
		return TUIStateFile{}, false, err
	}
	return file, true, nil
}

func loadTUIStateFileForUpdate(path string) (TUIStateFile, error) {
	file, _, err := readTUIStateFile(path)
	if err == nil {
		return file, nil
	}
	var newer *UnsupportedSchemaVersionError
	if errors.As(err, &newer) {
		return TUIStateFile{}, err
	}
	log.WarningLog.Printf("replacing unreadable TUI state file %s: %v", path, err)
	return defaultTUIStateFile(), nil
}

func decodeTUIStateFile(raw []byte, path string) (TUIStateFile, error) {
	plan := NewTUIStateSchemaMigrationPlan(path, validateTUIStateEnvelope)
	migrated, _, err := MigrateSchemaBytes(raw, plan)
	if err != nil {
		return TUIStateFile{}, err
	}
	var file TUIStateFile
	if err := json.Unmarshal(migrated, &file); err != nil {
		return TUIStateFile{}, err
	}
	if file.Repos == nil {
		file.Repos = make(map[string]TUIRepoViewState)
	}
	return file, nil
}

func writeTUIStateFile(path string, file TUIStateFile) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal TUI state: %w", err)
	}
	return AtomicWriteFile(path, data, 0644)
}

func defaultTUIStateFile() TUIStateFile {
	return TUIStateFile{
		SchemaVersion: TUIStateSchemaVersion,
		Repos:         make(map[string]TUIRepoViewState),
	}
}

func validateTUIStateEnvelope(raw []byte) error {
	var file TUIStateFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return err
	}
	if file.SchemaVersion != TUIStateSchemaVersion {
		return fmt.Errorf("schema_version = %d, want %d", file.SchemaVersion, TUIStateSchemaVersion)
	}
	if file.Repos == nil {
		return nil
	}
	for repoID := range file.Repos {
		if err := ValidateRepoID(repoID); err != nil {
			return fmt.Errorf("repo %q: %w", repoID, err)
		}
	}
	return nil
}
