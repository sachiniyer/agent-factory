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

var DefaultColumns = []string{"backlog", "in_progress", "review", "done"}

const tasksFileName = "tasks.json"

type Task struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Board struct {
	Columns []string `json:"columns"`
	Tasks   []Task   `json:"tasks"`
}

func (b *Board) AddTask(title, status string) Task {
	t := Task{
		ID:        generateID(),
		Title:     title,
		Status:    status,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	b.Tasks = append(b.Tasks, t)
	return t
}

func (b *Board) MoveTask(id, newStatus string) error {
	for i, t := range b.Tasks {
		if t.ID == id {
			b.Tasks[i].Status = newStatus
			b.Tasks[i].UpdatedAt = time.Now()
			return nil
		}
	}
	return fmt.Errorf("task with id %q not found", id)
}

func (b *Board) DeleteTask(id string) error {
	for i, t := range b.Tasks {
		if t.ID == id {
			b.Tasks = append(b.Tasks[:i], b.Tasks[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("task with id %q not found", id)
}

func (b *Board) GetTasksByStatus(status string) []Task {
	var result []Task
	for _, t := range b.Tasks {
		if t.Status == status {
			result = append(result, t)
		}
	}
	return result
}

func (b *Board) CountByStatus() map[string]int {
	counts := make(map[string]int)
	for _, t := range b.Tasks {
		counts[t.Status]++
	}
	return counts
}

func (b *Board) TaskCount() int {
	return len(b.Tasks)
}

func (b *Board) ToggleTask(id string) error {
	for i, t := range b.Tasks {
		if t.ID == id {
			if t.Status == "done" {
				b.Tasks[i].Status = "backlog"
			} else {
				b.Tasks[i].Status = "done"
			}
			b.Tasks[i].UpdatedAt = time.Now()
			return nil
		}
	}
	return fmt.Errorf("task with id %q not found", id)
}

// --- Load / Save ---

func tasksPath(repo *config.RepoContext) (string, error) {
	dir, err := repo.DataDir("tasks")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tasksFileName), nil
}

func LoadBoardForRepo(repo *config.RepoContext) (*Board, error) {
	path, err := tasksPath(repo)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Board{Columns: DefaultColumns, Tasks: []Task{}}, nil
		}
		return nil, fmt.Errorf("failed to read board file: %w", err)
	}
	var board Board
	if err := json.Unmarshal(data, &board); err != nil {
		return nil, fmt.Errorf("failed to parse board file: %w", err)
	}
	return &board, nil
}

func LoadBoard() (*Board, error) {
	repo, err := config.CurrentRepo()
	if err != nil {
		return nil, err
	}
	return LoadBoardForRepo(repo)
}

func SaveBoardForRepo(repo *config.RepoContext, board *Board) error {
	path, err := tasksPath(repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	data, err := json.MarshalIndent(board, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal board: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

func SaveBoard(board *Board) error {
	repo, err := config.CurrentRepo()
	if err != nil {
		return err
	}
	return SaveBoardForRepo(repo, board)
}

// --- Repo-scoped convenience (used by API) ---

func LoadTasksForRepo(repo *config.RepoContext) ([]Task, error) {
	board, err := LoadBoardForRepo(repo)
	if err != nil {
		return nil, err
	}
	return board.Tasks, nil
}

func AddTaskForRepoWithStatus(repo *config.RepoContext, title, status string) (Task, error) {
	board, err := LoadBoardForRepo(repo)
	if err != nil {
		return Task{}, err
	}
	t := board.AddTask(title, status)
	return t, SaveBoardForRepo(repo, board)
}

func ToggleTaskForRepo(repo *config.RepoContext, id string) error {
	board, err := LoadBoardForRepo(repo)
	if err != nil {
		return err
	}
	if err := board.ToggleTask(id); err != nil {
		return err
	}
	return SaveBoardForRepo(repo, board)
}

func DeleteTaskForRepo(repo *config.RepoContext, id string) error {
	board, err := LoadBoardForRepo(repo)
	if err != nil {
		return err
	}
	if err := board.DeleteTask(id); err != nil {
		return err
	}
	return SaveBoardForRepo(repo, board)
}

func MoveTaskForRepo(repo *config.RepoContext, id, newStatus string) error {
	board, err := LoadBoardForRepo(repo)
	if err != nil {
		return err
	}
	if err := board.MoveTask(id, newStatus); err != nil {
		return err
	}
	return SaveBoardForRepo(repo, board)
}

func generateID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
