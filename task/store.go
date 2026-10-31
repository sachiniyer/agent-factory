package task

// The tasks.json store layer: resolving the file's path, the load/migrate/stamp
// read path, and the locked write path every mutation funnels through.
//
// Split out of task.go — the same decomposition update.go began — so the Task
// model + trigger validation and the file that persists it are separately
// readable: this file owns getTasksPath/getTasksPathFn, MigrateOnLoadPath,
// LoadTasks/loadTasksLocked, ensureTasksSchemaMigrated, saveTasks/writeTasks,
// and the row-identity machinery (stampRowIdentity, storeGeneration, nowFn)
// that pins which version of the file a record was read from.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

const tasksFileName = "tasks.json"

// getTasksPathFn is the function used to resolve the tasks file path.
// It can be overridden in tests.
var getTasksPathFn = getTasksPath

func getTasksPath() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config directory: %w", err)
	}
	return filepath.Join(configDir, tasksFileName), nil
}

// SetTasksPathFnForTest swaps the tasks-file path resolver for tests in other
// packages. Returns a restore func. The default getTasksPath resolves
// tasks.json under AGENT_FACTORY_HOME; tests point it at a fixture file or
// return a synthetic error to inject a deterministic read failure through the
// active-scope (LoadTasksForKnownRepo) load path. The active-scope reloads in
// app/saveContentPaneState and app/handleTaskCreate no longer call
// config.CurrentRepo, so a non-git cwd can no longer trigger the read
// failure — a path-resolver error is the cleanest replacement.
func SetTasksPathFnForTest(f func() (string, error)) func() {
	prev := getTasksPathFn
	getTasksPathFn = f
	return func() { getTasksPathFn = prev }
}

// MigrateOnLoadPath returns the absolute path of tasks.json, which LoadTasks
// rewrites in place when it migrates a legacy file to the current schema at daemon
// load (task/schema_migration.go -> config.LoadAndMigrateSchemaFile). The upgrade
// transaction manifest (#2212 R3) snapshots it so a binary-only rollback restores
// tasks in a schema the previous daemon can read.
func MigrateOnLoadPath() (string, error) {
	return getTasksPathFn()
}

func LoadTasks() ([]Task, error) {
	path, err := getTasksPathFn()
	if err != nil {
		return nil, err
	}

	// A DANGLING tasks.json symlink reads as ENOENT, which os.Stat below would
	// take for "no store yet" and return an empty list — silently dropping every
	// schedule, watcher, and row the TUI and CLI show. The write path refuses
	// the same link (AtomicWriteFileFollowingLink → resolveWriteTarget), so the
	// read path must refuse it too, naming both ends: the read/write contract for
	// a followed file is symmetric (#3672 review). A simply-absent path stays an
	// ordinary empty store.
	if err := config.RefuseDanglingConfigLink(path); err != nil {
		return nil, err
	}

	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to read tasks file: %w", err)
		}
		// A resolving link's target can be removed between the guard above
		// and this Stat — the check-then-read window the config
		// materialization path closes the same way (#3672 review). Re-check
		// the link before treating the store as ordinarily absent, or a
		// link that dangled by the time Stat followed it reads as ENOENT
		// here and silently drops every task this guard was added to keep.
		if err := config.RefuseDanglingConfigLink(path); err != nil {
			return nil, err
		}
		// Lstat found an entry. A symlink whose target was restored between
		// the failed Stat and the recheck (another process replacing it via
		// two renames), or a regular file that replaced the link itself while
		// we weren't looking — either way the store may now be readable, so
		// retry the Stat before treating it as absent. A still-absent target
		// after the retry is a flapping link, not an empty store. Only a
		// path Lstat itself reports not-exist is an ordinary empty store
		// (#3672 review).
		if _, lerr := os.Lstat(path); lerr == nil {
			if _, err := os.Stat(path); err != nil {
				if os.IsNotExist(err) {
					if err := config.RefuseDanglingConfigLink(path); err != nil {
						return nil, err
					}
				}
				return nil, fmt.Errorf("failed to read tasks file: %w", err)
			}
		} else if os.IsNotExist(lerr) {
			return []Task{}, nil
		} else {
			return nil, fmt.Errorf("failed to read tasks file: %w", lerr)
		}
	}

	data, _, err := loadAndMigrateTasksFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tasks file: %w", err)
	}

	tasks, err := tasksFromSchemaBytes(data)
	if err != nil {
		return nil, err
	}
	// Every READ carries the schedule-health derivation (#3623). Deriving it here
	// rather than in each surface is what stops the next surface from being the
	// one that renders a dark task as healthy. loadTasksLocked — the WRITE path's
	// loader — deliberately does not, so nothing derived can reach disk.
	return stampRowIdentity(WithScheduleHealth(tasks, nowFn()), storeGeneration(data)), nil
}

// stampRowIdentity gives every record the two halves of its row identity: the
// generation of the bytes this read decoded, and its 1-based position within
// them. See Task.Ordinal and Task.StoreGeneration.
//
// One helper for both halves on purpose. They are meaningless apart — a row
// number without the version it was read in is the bug #3684 found — so nothing
// should be able to stamp one and forget the other.
//
// It runs LAST on each read path — after WithScheduleHealth here, and after the
// strip in loadTasksLocked — so a record's identity always describes the read
// that produced it and never a value the file supplied. A hand-typed "ordinal"
// or "store_generation" is therefore overwritten rather than believed, which is
// the rule the health fields follow and for the same reason.
//
// WRITE paths call it too, with the generation writeTasks reports, and they have
// to. A write loads under the lock, and loadTasksLocked stamps what it READ —
// the pre-write bytes. Mutate and save, and every record the path hands back
// names a version of the store that no longer exists, so it can never pair with
// a later read and a consumer merging a mutation response or an EventTaskUpdated
// record into its list would silently lose that row's arming (#3684 review).
//
// Re-derived over the whole written slice rather than patched onto one record,
// because a write can move rows: replacing the generation while leaving a stale
// ordinal would produce an identity that MATCHES and is wrong, which is the one
// failure direction worth spending code to avoid.
func stampRowIdentity(tasks []Task, generation string) []Task {
	for i := range tasks {
		tasks[i].Ordinal = i + 1
		tasks[i].StoreGeneration = generation
	}
	return tasks
}

// storeGeneration tags one version of the store, so two reads can tell whether
// they saw the same bytes.
//
// Truncated deliberately. This is an EQUALITY TAG for two reads taken seconds
// apart, not a security primitive: the only question asked of it is "are these
// the same file contents", and anyone who could aim a collision at it can edit
// tasks.json directly and skip the trouble. Sixty-four bits puts an accidental
// match between two specific stores at about one in 1.8e19, and keeps the tag
// short enough to ride every record of every ListTasks response.
func storeGeneration(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// nowFn is the clock the read-time derivation reads. A package variable so tests
// can pin it; production reads the wall clock.
var nowFn = time.Now

// loadTasksLocked reads tasks while the caller already holds path's file lock.
// It must not call LoadTasks because LoadTasks may migrate and acquire the same
// lock. If a legacy array sneaks in between the pre-lock migration and this
// read, saveTasks will still write the updated v1 envelope.
func loadTasksLocked(path string) ([]Task, error) {
	// The same refusal LoadTasks makes. This is the WRITE path's loader — the
	// mutation callers (AddTask, UpdateTask, RemoveTask) load here to find the
	// row they change. A dangling link made it return an empty list, so every
	// mutation then reported "task with id X not found" for a task that does
	// exist in the unreachable target, and the write-side refusal was never
	// reached. Refusing here surfaces the real cause before that misleading
	// "not found" can (#3672 review).
	if err := config.RefuseDanglingConfigLink(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to read tasks file: %w", err)
		}
		// The same check-then-read race as LoadTasks: a target removed
		// between the guard and this ReadFile re-creates the empty list
		// that hides a dangling link. Re-check before returning the
		// ordinary-empty shape, so a mutation against a now-dangling link
		// surfaces the broken link instead of a misleading "not found"
		// (#3672 review).
		if err := config.RefuseDanglingConfigLink(path); err != nil {
			return nil, err
		}
		// The same recheck-then-retry race as LoadTasks: a resolving link
		// whose target was restored between the failed ReadFile and the
		// recheck passes RefuseDanglingConfigLink and would read as an empty
		// store, and a regular file that replaced the link while we weren't
		// looking is just as readable. Retry the read whenever Lstat finds
		// any entry; a still-absent target after the retry is a flapping
		// link, not an empty store. Only a path Lstat itself reports
		// not-exist is the ordinary empty shape (#3672 review).
		if _, lerr := os.Lstat(path); lerr == nil {
			data, err = os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					if err := config.RefuseDanglingConfigLink(path); err != nil {
						return nil, err
					}
				}
				return nil, fmt.Errorf("failed to read tasks file: %w", err)
			}
		} else if os.IsNotExist(lerr) {
			return []Task{}, nil
		} else {
			return nil, fmt.Errorf("failed to read tasks file: %w", lerr)
		}
	}
	migrated, _, err := migrateTasksSchemaBytes(data, path)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tasks file: %w", err)
	}
	tasks, err := tasksFromSchemaBytes(migrated)
	if err != nil {
		return nil, err
	}
	// Derived state is stripped here as well as on the way out (saveTasks), and
	// for a reason the write path does not make obvious: records loaded here do
	// not only go back to disk. They ride EventTaskUpdated to every connected
	// client — the legacy RepoID backfill publishes them, and so does an update
	// whose post-commit reload failed — so a hand-edited tasks.json carrying
	// `arming` or `next_run_at` would have those fields decoded and broadcast as
	// though a daemon had observed them (#3623 review).
	//
	// The write path derives nothing, so clearing is the whole of it: a reader
	// gets its verdict from LoadTasks, which derives, and every other exit
	// carries none.
	for i := range tasks {
		tasks[i].stripDerived()
	}
	// Identified after the strip, not before it: stripDerived clears the row
	// identity along with the rest, and this path's records do not only go back to
	// disk either. LoadTasksWithStableRepoBindingUpdates returns exactly this
	// slice as its authoritative list, so leaving it unstamped would hand a future
	// caller a set of rows that all claim to be unidentified — the one shape
	// ApplyLiveArming reads as "nothing to pair on" and refuses. Every read path
	// returns identified rows; only the write path carries none (#3680).
	return stampRowIdentity(tasks, storeGeneration(migrated)), nil
}

func ensureTasksSchemaMigrated(path string) error {
	// A dangling link made os.Stat return ENOENT, which skipped migration with
	// nil error — so the daemon-load schema check passed a broken link as a
	// migrated store, the same silent-empty shape LoadTasks and loadTasksLocked
	// produced. Refuse it the same way they do (#3672 review).
	if err := config.RefuseDanglingConfigLink(path); err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read tasks file: %w", err)
		}
		// The same check-then-read race as LoadTasks: re-check the link
		// before treating the store as absent, or a target removed between
		// the guard and this Stat skips migration as if the store were gone
		// — the silent-empty shape this guard was added to stop (#3672
		// review).
		if err := config.RefuseDanglingConfigLink(path); err != nil {
			return err
		}
		// The same recheck-then-retry race as LoadTasks: a resolving link
		// whose target was restored between the failed Stat and the recheck
		// passes RefuseDanglingConfigLink and would skip migration as if the
		// store were absent, and a regular file that replaced the link while
		// we weren't looking is just as readable. Retry the Stat whenever
		// Lstat finds any entry; a still-absent target after the retry is a
		// flapping link, not an absent store. Only a path Lstat itself
		// reports not-exist stays the ordinary nil (#3672 review).
		if _, lerr := os.Lstat(path); lerr == nil {
			if _, err := os.Stat(path); err != nil {
				if os.IsNotExist(err) {
					if err := config.RefuseDanglingConfigLink(path); err != nil {
						return err
					}
				}
				return fmt.Errorf("failed to read tasks file: %w", err)
			}
		} else if os.IsNotExist(lerr) {
			return nil
		} else {
			return fmt.Errorf("failed to read tasks file: %w", lerr)
		}
	}
	_, _, err := loadAndMigrateTasksFile(path)
	if err != nil {
		return fmt.Errorf("failed to parse tasks file: %w", err)
	}
	return nil
}

// saveTasks writes tasks without locking. Must be called from within WithFileLock.
//
// It strips the read-time derived fields on the way out (#3623). Every load now
// populates them, so a load-modify-save path would otherwise store a stale
// "overdue" — a claim about an instant that has already passed by the time
// anything reads it back. Enforcing it in the ONE writer rather than in each
// caller is what makes "derived, never persisted" a property instead of a
// convention.
func saveTasks(tasks []Task) error {
	_, err := writeTasks(tasks)
	return err
}

// writeTasks is saveTasks, also reporting the generation of the bytes it wrote.
//
// Only the create path needs it, and it needs it for a reason worth stating: the
// record AddTaskChecked hands back is published as EventTaskCreated, and a
// generation is a property of the file as a whole, so the appended row cannot
// know its own until the write that produces it has been marshalled. Reading it
// off the write is exact and costs nothing; re-reading the file afterwards would
// be a second read that a concurrent writer could make disagree.
func writeTasks(tasks []Task) (string, error) {
	noteTaskLayout(tasks)
	path, err := getTasksPathFn()
	if err != nil {
		return "", err
	}
	stored := make([]Task, len(tasks))
	copy(stored, tasks)
	for i := range stored {
		stored[i].stripDerived()
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal tasks: %w", err)
	}
	enveloped, err := marshalTasksEnvelope(data)
	if err != nil {
		return "", fmt.Errorf("failed to marshal tasks envelope: %w", err)
	}
	// tasks.json FOLLOWS a symlink (#3672). It is the one user-authored store in
	// the af home — people write it by hand and a user may reasonably keep it in
	// dotfiles beside config.toml — so it joins the global config on the follow
	// side rather than af's own managed files on the refuse side. The link stays;
	// the file it names is rewritten.
	//
	// The LOCK deliberately stays on af's own path (config.WithFileLock above,
	// not the followed variant the global config uses). Two consequences, both
	// wanted: af's .lock never lands in the user's dotfiles working tree, and
	// every af process sharing one AF home locks the same file whatever the link
	// points at. What it does not cover is two DIFFERENT AF homes aliasing one
	// tasks.json — they would lock two paths and write one file. That is the
	// #3697 class (the followed lock's own resolution), it is being reworked in
	// #3696, and this change deliberately does not touch that machinery.
	if err := config.AtomicWriteFileFollowingLink(path, enveloped, 0644); err != nil {
		return "", err
	}
	return storeGeneration(enveloped), nil
}
