package task

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

const tasksFileName = "tasks.json"

// taskIDPattern restricts a task ID to characters that are safe to use as a
// single path segment. Legitimate IDs from GenerateID are 8 lowercase hex
// characters; the wider class accommodates any future ID scheme while
// preventing path-traversal segments like "..", "/", or "\".
var taskIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// maxTaskIDLength caps the size of an accepted task ID. Legitimate IDs are
// 8 chars; the cap is loose enough for future schemes while bounding the
// size of values that flow into filesystem paths and error messages.
const maxTaskIDLength = 128

// ValidateTaskID enforces the shape of a task identifier before it is used
// to construct filesystem paths (lock files, log files, scheduler units).
// Returns an error when the id is empty, exceeds maxTaskIDLength, or
// contains any character outside [a-zA-Z0-9_-] — in particular "." (used
// in traversal), "/", or "\". Mirrors config.ValidateRepoID.
func ValidateTaskID(taskID string) error {
	if taskID == "" {
		return fmt.Errorf("invalid task id: empty")
	}
	if len(taskID) > maxTaskIDLength {
		return fmt.Errorf("invalid task id: length %d exceeds maximum %d", len(taskID), maxTaskIDLength)
	}
	if !taskIDPattern.MatchString(taskID) {
		return fmt.Errorf("invalid task id: must match %s", taskIDPattern.String())
	}
	return nil
}

type Task struct {
	ID            string     `json:"id"`
	Name          string     `json:"name,omitempty"`
	Prompt        string     `json:"prompt"`
	CronExpr      string     `json:"cron_expr"`
	ProjectPath   string     `json:"project_path"`
	Program       string     `json:"program"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastRunStatus string     `json:"last_run_status,omitempty"`
}

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

func LoadTasks() ([]Task, error) {
	path, err := getTasksPathFn()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Task{}, nil
		}
		return nil, fmt.Errorf("failed to read tasks file: %w", err)
	}

	var tasks []Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("failed to parse tasks file: %w", err)
	}

	return tasks, nil
}

func SaveTasks(tasks []Task) error {
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal tasks: %w", err)
	}

	return config.WithFileLock(path, func() error {
		return config.AtomicWriteFile(path, data, 0644)
	})
}

// saveTasks writes tasks without locking. Must be called from within WithFileLock.
func saveTasks(tasks []Task) error {
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal tasks: %w", err)
	}
	return config.AtomicWriteFile(path, data, 0644)
}

func AddTask(t Task) error {
	if err := ValidateTaskID(t.ID); err != nil {
		return err
	}
	// Empty Program means "fall back to the configured default_program at
	// run time"; only validate when an explicit per-task override was set.
	if t.Program != "" {
		if err := config.ValidateProgramEnum("task program", t.Program); err != nil {
			return err
		}
	}
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		tasks, err := LoadTasks()
		if err != nil {
			return err
		}
		tasks = append(tasks, t)
		return saveTasks(tasks)
	})
}

func RemoveTask(id string) error {
	if err := ValidateTaskID(id); err != nil {
		return err
	}
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		tasks, err := LoadTasks()
		if err != nil {
			return err
		}

		filtered := make([]Task, 0, len(tasks))
		found := false
		for _, t := range tasks {
			if t.ID == id {
				found = true
				continue
			}
			filtered = append(filtered, t)
		}

		if !found {
			return fmt.Errorf("task with id %q not found", id)
		}

		return saveTasks(filtered)
	})
}

func GetTask(id string) (*Task, error) {
	if err := ValidateTaskID(id); err != nil {
		return nil, err
	}
	tasks, err := LoadTasks()
	if err != nil {
		return nil, err
	}

	for _, t := range tasks {
		if t.ID == id {
			return &t, nil
		}
	}

	return nil, fmt.Errorf("task with id %q not found", id)
}

func GenerateID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// LoadTasksForCurrentRepo returns only tasks whose ProjectPath
// matches the current git repository root.
func LoadTasksForCurrentRepo() ([]Task, error) {
	repo, err := config.CurrentRepo()
	if err != nil {
		return nil, err
	}
	all, err := LoadTasks()
	if err != nil {
		return nil, err
	}
	var filtered []Task
	for _, t := range all {
		if t.ProjectPath == repo.Root {
			filtered = append(filtered, t)
		}
	}
	return filtered, nil
}

func UpdateTask(t Task) error {
	if err := ValidateTaskID(t.ID); err != nil {
		return err
	}
	// Empty Program means "fall back to the configured default_program at
	// run time"; only validate when an explicit per-task override was set.
	if t.Program != "" {
		if err := config.ValidateProgramEnum("task program", t.Program); err != nil {
			return err
		}
	}
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		tasks, err := LoadTasks()
		if err != nil {
			return err
		}

		found := false
		for i, existing := range tasks {
			if existing.ID == t.ID {
				tasks[i] = t
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("task with id %q not found", t.ID)
		}

		return saveTasks(tasks)
	})
}
