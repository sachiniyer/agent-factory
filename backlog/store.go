package backlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

const (
	BacklogFileName      = "backlog.json"
	BudgetFileName       = "budget.json"
	BacklogSchemaVersion = 2
	BudgetSchemaVersion  = 1
)

// ErrBusy is returned when another writer held a store's lock too long. The
// change was not made; retrying is safe.
var ErrBusy = errors.New("backlog changed while saving; retry")

// lockTimeout bounds every store lock. A var so tests can shorten it.
var lockTimeout = 5 * time.Second

// BacklogPath returns $AF_HOME/repos/<repoID>/backlog.json.
func BacklogPath(repoID string) (string, error) {
	if err := config.ValidateRepoID(repoID); err != nil {
		return "", err
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config dir: %w", err)
	}
	parent := filepath.Join(home, "repos")
	dir := filepath.Join(parent, repoID)
	if !strings.HasPrefix(filepath.Clean(dir)+string(filepath.Separator), filepath.Clean(parent)+string(filepath.Separator)) {
		return "", errors.New("invalid repo id: resolved path escapes repos directory")
	}
	return filepath.Join(dir, BacklogFileName), nil
}

// BudgetPath returns $AF_HOME/budget.json.
func BudgetPath() (string, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config dir: %w", err)
	}
	return filepath.Join(home, BudgetFileName), nil
}

// Load reads a project's backlog. A missing file is an empty backlog.
func Load(repoID string) (*Backlog, error) {
	path, err := BacklogPath(repoID)
	if err != nil {
		return nil, err
	}
	return loadFile(path, backlogStore)
}

// Update runs fn on the project's backlog under its file lock and saves the
// result when fn returns nil.
func Update(repoID string, fn func(*Backlog) error) (*Backlog, error) {
	path, err := BacklogPath(repoID)
	if err != nil {
		return nil, err
	}
	return updateFile(path, backlogStore, fn)
}

// Budget is the share of the user's Claude plan the runner may use, for all
// projects. Percents are of a plan window; USDLimit is a dollar limit at API
// prices for accounts with no plan readings.
type Budget struct {
	SchemaVersion int     `json:"schema_version"`
	Share         float64 `json:"share"`
	StopAt        float64 `json:"stop_at"`
	MaxPerRun     float64 `json:"max_per_run"`
	USDLimit      float64 `json:"usd_limit"`
	USDSpent      float64 `json:"usd_spent"`
}

const (
	DefaultShare     = 50
	DefaultStopAt    = 90
	DefaultMaxPerRun = 10
)

func NewBudget() *Budget {
	return &Budget{SchemaVersion: BudgetSchemaVersion, Share: DefaultShare, StopAt: DefaultStopAt, MaxPerRun: DefaultMaxPerRun}
}

// LoadBudget reads the global budget. A missing file is the default budget.
func LoadBudget() (*Budget, error) {
	path, err := BudgetPath()
	if err != nil {
		return nil, err
	}
	return loadFile(path, budgetStore)
}

// UpdateBudget runs fn on the global budget under its file lock and saves the
// result when fn returns nil.
func UpdateBudget(fn func(*Budget) error) (*Budget, error) {
	path, err := BudgetPath()
	if err != nil {
		return nil, err
	}
	return updateFile(path, budgetStore, fn)
}

type store[T any] struct {
	name     string
	version  int
	empty    func() *T
	finish   func(*T) error
	setStamp func(*T, int)
	// migrate holds the migrators from version 1 up, by source version.
	migrate map[int]config.SchemaMigrator
}

var backlogStore = store[Backlog]{
	name:     BacklogFileName,
	version:  BacklogSchemaVersion,
	empty:    New,
	finish:   (*Backlog).normalize,
	setStamp: func(b *Backlog, v int) { b.SchemaVersion = v },
	// Version 2 only adds fields (paused and archived on items, then and handoff
	// on messages), so its migrator only stamps. The bump stops an older af from
	// saving the file and dropping them.
	migrate: map[int]config.SchemaMigrator{1: stampVersion(2)},
}

var budgetStore = store[Budget]{
	name:     BudgetFileName,
	version:  BudgetSchemaVersion,
	empty:    NewBudget,
	finish:   func(*Budget) error { return nil },
	setStamp: func(b *Budget, v int) { b.SchemaVersion = v },
}

func (s store[T]) plan(path string) config.SchemaMigrationPlan {
	reg := config.NewSchemaMigrationRegistry()
	if err := reg.Register(config.LegacySchemaVersion, stampVersion(1)); err != nil {
		panic(err)
	}
	for from, m := range s.migrate {
		if err := reg.Register(from, m); err != nil {
			panic(err)
		}
	}
	return config.SchemaMigrationPlan{
		StoreName:           s.name,
		Path:                path,
		CurrentVersion:      s.version,
		DetectVersion:       config.DetectJSONSchemaVersion,
		Migrators:           reg,
		Validate:            s.validate,
		Perm:                0o644,
		ProveCurrentVersion: config.ProveJSONSchemaVersion,
	}
}

// stampVersion migrates a file written without schema_version (by hand) to v.
func stampVersion(v int) config.SchemaMigrator {
	return func(raw []byte) ([]byte, error) {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		if obj == nil {
			return nil, errors.New("the file must hold a JSON object")
		}
		obj[config.SchemaVersionField] = json.RawMessage(fmt.Sprint(v))
		return json.MarshalIndent(obj, "", "  ")
	}
}

func (s store[T]) validate(raw []byte) error {
	_, err := s.decode(raw)
	return err
}

func (s store[T]) decode(raw []byte) (*T, error) {
	v := s.empty()
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, err
	}
	if err := s.finish(v); err != nil {
		return nil, err
	}
	return v, nil
}

func loadFile[T any](path string, s store[T]) (*T, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return s.empty(), nil
		}
		return nil, err
	}
	raw, _, err := config.LoadAndMigrateSchemaFile(s.plan(path))
	if err != nil {
		return nil, wrapBusy(err)
	}
	return s.decode(raw)
}

// updateFile is the one write path. MigrateSchemaBytes refuses a file whose
// schema_version is newer than this binary knows, or unreadable, so an older
// af never overwrites a newer af's file.
func updateFile[T any](path string, s store[T], fn func(*T) error) (*T, error) {
	if err := config.MkdirAllUnderAFHome(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", filepath.Dir(path), err)
	}
	var out *T
	err := config.WithFileLockTimeout(path, lockTimeout, func() error {
		v := s.empty()
		raw, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
		case err != nil:
			return err
		default:
			migrated, _, err := config.MigrateSchemaBytes(raw, s.plan(path))
			if err != nil {
				return err
			}
			if v, err = s.decode(migrated); err != nil {
				return err
			}
		}
		if err := fn(v); err != nil {
			return err
		}
		s.setStamp(v, s.version)
		if err := s.finish(v); err != nil {
			return err
		}
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		if err := config.AtomicWriteFile(path, append(data, '\n'), 0o644); err != nil {
			return err
		}
		out = v
		return nil
	})
	if err != nil {
		return nil, wrapBusy(err)
	}
	return out, nil
}

func wrapBusy(err error) error {
	if errors.Is(err, config.ErrLockTimeout) {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	return err
}
