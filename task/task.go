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

func getTasksPath() (string, error) {
	repo, err := config.CurrentRepo()
	if err != nil {
		return "", err
	}
	dir, err := repo.DataDir("tasks")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tasksFileName), nil
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
