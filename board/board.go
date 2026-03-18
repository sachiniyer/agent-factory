package board

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/sachiniyer/agent-factory/config"

	"os"
	"path/filepath"
	"strings"
	"time"
)

var DefaultColumns = []string{"backlog", "in_progress", "review", "done"}

const tasksFileName = "tasks.json"

type Task struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Status        string    `json:"status"`
	InstanceTitle string    `json:"instance_title,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
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

// LinkTask links a task to an instance by title.
func (b *Board) LinkTask(taskID, instanceTitle string) error {
	for i, t := range b.Tasks {
		if t.ID == taskID {
			b.Tasks[i].InstanceTitle = instanceTitle
			b.Tasks[i].UpdatedAt = time.Now()
			return nil
		}
	}
	return fmt.Errorf("task with id %q not found", taskID)
}

// UnlinkTask removes the instance linkage from a task.
func (b *Board) UnlinkTask(taskID string) error {
	for i, t := range b.Tasks {
		if t.ID == taskID {
			b.Tasks[i].InstanceTitle = ""
			b.Tasks[i].UpdatedAt = time.Now()
			return nil
		}
	}
	return fmt.Errorf("task with id %q not found", taskID)
}

// GetTaskByID returns the task with the given ID, or nil if not found.
func (b *Board) GetTaskByID(id string) *Task {
	for i, t := range b.Tasks {
		if t.ID == id {
			return &b.Tasks[i]
		}
	}
	return nil
}

// FindTaskByInstance returns the first task linked to the given instance title, or nil.
func (b *Board) FindTaskByInstance(instanceTitle string) *Task {
	for i, t := range b.Tasks {
		if t.InstanceTitle == instanceTitle {
			return &b.Tasks[i]
		}
	}
	return nil
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

// updateBoardForRepo loads the board, applies fn, and saves it back.
func updateBoardForRepo(repo *config.RepoContext, fn func(*Board) error) error {
	board, err := LoadBoardForRepo(repo)
	if err != nil {
		return err
	}
	if err := fn(board); err != nil {
		return err
	}
	return SaveBoardForRepo(repo, board)
}

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
	return updateBoardForRepo(repo, func(b *Board) error { return b.ToggleTask(id) })
}

func DeleteTaskForRepo(repo *config.RepoContext, id string) error {
	return updateBoardForRepo(repo, func(b *Board) error { return b.DeleteTask(id) })
}

func MoveTaskForRepo(repo *config.RepoContext, id, newStatus string) error {
	return updateBoardForRepo(repo, func(b *Board) error { return b.MoveTask(id, newStatus) })
}

func LinkTaskForRepo(repo *config.RepoContext, taskID, instanceTitle string) error {
	return updateBoardForRepo(repo, func(b *Board) error { return b.LinkTask(taskID, instanceTitle) })
}

func UnlinkTaskForRepo(repo *config.RepoContext, taskID string) error {
	return updateBoardForRepo(repo, func(b *Board) error { return b.UnlinkTask(taskID) })
}

func generateID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// GenerateInstanceTitle creates a short, unique session title from a task description.
// existingTitles is the set of titles already in use.
func GenerateInstanceTitle(taskTitle string, existingTitles map[string]bool) string {
	base := strings.TrimSpace(taskTitle)
	if len(base) > 24 {
		base = base[:24]
		if idx := strings.LastIndex(base, " "); idx > 8 {
			base = base[:idx]
		}
	}
	base = strings.TrimSpace(base)
	if base == "" {
		base = "board-task"
	}

	candidate := base
	if !existingTitles[candidate] {
		return candidate
	}
	for i := 2; i < 100; i++ {
		candidate = fmt.Sprintf("%s-%d", base, i)
		if !existingTitles[candidate] {
			return candidate
		}
	}
	return candidate
}
