package task

import (
	"claude-squad/config"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const tasksFileName = "tasks.json"

type Task struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"created_at"`
}

// getTasksPathForRepo returns the tasks file path for a specific repo context.
func getTasksPathForRepo(repo *config.RepoContext) (string, error) {
	dir, err := repo.DataDir("tasks")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tasksFileName), nil
}

// LoadTasksForRepo loads tasks for a specific repo context.
func LoadTasksForRepo(repo *config.RepoContext) ([]Task, error) {
	path, err := getTasksPathForRepo(repo)
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

func LoadTasks() ([]Task, error) {
	repo, err := config.CurrentRepo()
	if err != nil {
		return nil, err
	}
	return LoadTasksForRepo(repo)
}

// SaveTasksForRepo saves tasks for a specific repo context.
func SaveTasksForRepo(repo *config.RepoContext, tasks []Task) error {
	path, err := getTasksPathForRepo(repo)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal tasks: %w", err)
	}

	return os.WriteFile(path, data, 0644)
}

func SaveTasks(tasks []Task) error {
	repo, err := config.CurrentRepo()
	if err != nil {
		return err
	}
	return SaveTasksForRepo(repo, tasks)
}

// AddTaskForRepo adds a task for a specific repo context.
func AddTaskForRepo(repo *config.RepoContext, title string) (Task, error) {
	tasks, err := LoadTasksForRepo(repo)
	if err != nil {
		return Task{}, err
	}

	t := Task{
		ID:        GenerateID(),
		Title:     title,
		Done:      false,
		CreatedAt: time.Now(),
	}
	tasks = append(tasks, t)
	return t, SaveTasksForRepo(repo, tasks)
}

// ToggleTaskForRepo toggles a task's done status for a specific repo context.
func ToggleTaskForRepo(repo *config.RepoContext, id string) error {
	tasks, err := LoadTasksForRepo(repo)
	if err != nil {
		return err
	}

	found := false
	for i, t := range tasks {
		if t.ID == id {
			tasks[i].Done = !tasks[i].Done
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("task with id %q not found", id)
	}

	return SaveTasksForRepo(repo, tasks)
}

// DeleteTaskForRepo deletes a task for a specific repo context.
func DeleteTaskForRepo(repo *config.RepoContext, id string) error {
	tasks, err := LoadTasksForRepo(repo)
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

	return SaveTasksForRepo(repo, filtered)
}

func GenerateID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
