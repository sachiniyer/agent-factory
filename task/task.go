package task

import (
	"claude-squad/config"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const tasksFileName = "tasks.json"

type Task struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"created_at"`
}

// getRepoID returns a short hash identifying the current git repo based on its root path.
func getRepoID() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git repo root: %w", err)
	}
	root := strings.TrimSpace(string(out))
	hash := sha256.Sum256([]byte(root))
	return hex.EncodeToString(hash[:6]), nil
}

func getTasksPath() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config directory: %w", err)
	}
	repoID, err := getRepoID()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "tasks", repoID, tasksFileName), nil
}

func LoadTasks() ([]Task, error) {
	path, err := getTasksPath()
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
	path, err := getTasksPath()
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

func AddTask(title string) error {
	tasks, err := LoadTasks()
	if err != nil {
		return err
	}

	tasks = append(tasks, Task{
		ID:        GenerateID(),
		Title:     title,
		Done:      false,
		CreatedAt: time.Now(),
	})
	return SaveTasks(tasks)
}

func UpdateTask(id, title string) error {
	tasks, err := LoadTasks()
	if err != nil {
		return err
	}

	found := false
	for i, t := range tasks {
		if t.ID == id {
			tasks[i].Title = title
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("task with id %q not found", id)
	}

	return SaveTasks(tasks)
}

func ToggleTask(id string) error {
	tasks, err := LoadTasks()
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

	return SaveTasks(tasks)
}

func DeleteTask(id string) error {
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

	return SaveTasks(filtered)
}

func GenerateID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
