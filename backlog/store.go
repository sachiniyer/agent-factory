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
	BacklogFileName = "backlog.json"
	BudgetFileName  = "budget.json"
	// BacklogSchemaVersion is the newest backlog.json this af reads. A save writes
	// it only while some group has an orchestrator, and BacklogUnboundSchemaVersion
	// otherwise (Backlog.savedSchemaVersion).
	BacklogSchemaVersion        = 3
	BacklogUnboundSchemaVersion = 2
	BudgetSchemaVersion         = 2
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

// Budget is when the runner may use the user's Claude plan, for all projects.
// Limit is the total plan % (from any use) under which the runner starts
// items, in both plan windows; MaxPerRun is a % of the 5-hour window. USDLimit
// is a dollar limit at API prices for accounts with no plan readings.
type Budget struct {
	SchemaVersion int     `json:"schema_version"`
	Limit         float64 `json:"limit"`
	MaxPerRun     float64 `json:"max_per_run"`
	USDLimit      float64 `json:"usd_limit"`
	USDSpent      float64 `json:"usd_spent"`
}

const (
	DefaultLimit     = 50
	DefaultMaxPerRun = 10
)

func NewBudget() *Budget {
	return &Budget{SchemaVersion: BudgetSchemaVersion, Limit: DefaultLimit, MaxPerRun: DefaultMaxPerRun}
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
	name string
	// version is the newest schema version this binary reads, and what a save
	// writes unless stamp says otherwise.
	version int
	// stamp, when set, is the version a save writes, from the data: the
	// backlog writes its newest version only while it holds data an older af
	// could not represent. Its version on disk changes only on a save, so a
	// read migrates in memory and leaves the file alone.
	stamp    func(*T) int
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
	stamp:    (*Backlog).savedSchemaVersion,
	// Versions 2 and 3 only add fields (2: paused and archived on items, then
	// and handoff on messages; 3: a group's orchestrator), so their migrators
	// only stamp. A save writes 3 only while a group has an orchestrator, so an
	// af that knows only version 2 refuses a file holding one rather than
	// dropping it, and reads every other file as before.
	migrate: map[int]config.SchemaMigrator{1: stampVersion(2), 2: stampVersion(3)},
}

var budgetStore = store[Budget]{
	name:     BudgetFileName,
	version:  BudgetSchemaVersion,
	empty:    NewBudget,
	finish:   func(*Budget) error { return nil },
	setStamp: func(b *Budget, v int) { b.SchemaVersion = v },
	migrate:  map[int]config.SchemaMigrator{1: budgetToLimit},
}

// The share and stop_at defaults of a version 1 budget, for a file that left
// one out.
const (
	v1DefaultShare  = 50
	v1DefaultStopAt = 90
)

// budgetToLimit migrates a version 1 budget, which held the backlog to a share
// of each plan window and stopped the runner at a stop_at on total plan use, to
// one limit on total plan use: the lower of the two. That never starts an item
// the old budget would have held. Total plan use is at least the backlog's part
// of it, so it reaches the lower of the two whenever either old gate closed. A
// hand-written file with no schema_version that already has a limit keeps it.
func budgetToLimit(raw []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("the file must hold a JSON object")
	}
	_, hasShare := obj["share"]
	_, hasStopAt := obj["stop_at"]
	if _, hasLimit := obj["limit"]; !hasLimit || hasShare || hasStopAt {
		share, err := v1Percent(obj, "share", v1DefaultShare)
		if err != nil {
			return nil, err
		}
		stopAt, err := v1Percent(obj, "stop_at", v1DefaultStopAt)
		if err != nil {
			return nil, err
		}
		l, err := json.Marshal(min(share, stopAt))
		if err != nil {
			return nil, err
		}
		obj["limit"] = l
	}
	delete(obj, "share")
	delete(obj, "stop_at")
	obj[config.SchemaVersionField] = json.RawMessage("2")
	return json.MarshalIndent(obj, "", "  ")
}

// v1Percent reads one percent of a version 1 budget, or def when it is absent.
func v1Percent(obj map[string]json.RawMessage, key string, def float64) (float64, error) {
	raw, ok := obj[key]
	if !ok {
		return def, nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
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
	if s.stamp != nil {
		// The version on disk follows the data and changes only on a save:
		// migrate in memory, and report the version a save would write.
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: read: %w", s.name, err)
		}
		migrated, _, err := config.MigrateSchemaBytes(raw, s.plan(path))
		if err != nil {
			return nil, err
		}
		v, err := s.decode(migrated)
		if err != nil {
			return nil, err
		}
		s.setStamp(v, s.stamp(v))
		return v, nil
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
		// finish first: it can drop data (an invalid binding) that decides the stamp.
		if err := s.finish(v); err != nil {
			return err
		}
		version := s.version
		if s.stamp != nil {
			version = s.stamp(v)
		}
		s.setStamp(v, version)
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
