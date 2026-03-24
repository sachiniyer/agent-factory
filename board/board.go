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
	Columns   []string  `json:"columns"`
	Tasks     []Task    `json:"tasks"`
	UpdatedAt time.Time `json:"updated_at"`
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
	board.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(board, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal board: %w", err)
	}
	return config.WithFileLock(path, func() error {
		return config.AtomicWriteFile(path, data, 0644)
	})
}

// MergeBoards merges external changes from disk into the user's edited board.
// User edits take priority. New tasks from disk are added. Tasks deleted by the
// user stay deleted. Tasks modified on disk but not by the user get the disk version.
//
// originalIDs is the set of task IDs that were present when the user's board was
// loaded from disk. This lets us distinguish "user deleted a task" (ID was in
// originalIDs but is no longer in userBoard) from "new external task" (ID was
// NOT in originalIDs). If originalIDs is nil, all disk tasks not in userBoard
// are treated as new.
func MergeBoards(userBoard, diskBoard *Board, originalIDs map[string]bool) *Board {
	if userBoard == nil {
		return diskBoard
	}
	if diskBoard == nil {
		return userBoard
	}

	// Build a set of task IDs in userBoard for fast lookup.
	userIDs := make(map[string]bool, len(userBoard.Tasks))
	for _, t := range userBoard.Tasks {
		userIDs[t.ID] = true
	}

	// Start with the user's tasks (preserving their order and edits).
	merged := &Board{
		Columns:   userBoard.Columns,
		Tasks:     make([]Task, len(userBoard.Tasks)),
		UpdatedAt: userBoard.UpdatedAt,
	}
	copy(merged.Tasks, userBoard.Tasks)

	// Add tasks from disk that don't exist in user board, but only if they
	// are truly new (not present in the original loaded set, meaning the
	// user didn't delete them).
	for _, dt := range diskBoard.Tasks {
		if userIDs[dt.ID] {
			continue // already in user board — user version wins
		}
		if originalIDs != nil && originalIDs[dt.ID] {
			continue // was in original load — user deleted it, honour that
		}
		// Truly new external task — add it.
		merged.Tasks = append(merged.Tasks, dt)
	}

	return merged
}

func SaveBoard(board *Board) error {
	repo, err := config.CurrentRepo()
	if err != nil {
		return err
	}
	return SaveBoardForRepo(repo, board)
}

// --- Repo-scoped convenience (used by API) ---

// updateBoardForRepo loads the board under a file lock, applies fn, and saves it back atomically.
func updateBoardForRepo(repo *config.RepoContext, fn func(*Board) error) error {
	path, err := tasksPath(repo)
	if err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		board, err := LoadBoardForRepo(repo)
		if err != nil {
			return err
		}
		if err := fn(board); err != nil {
			return err
		}
		// Save directly (already under lock).
		board.UpdatedAt = time.Now()
		data, err := json.MarshalIndent(board, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal board: %w", err)
		}
		return config.AtomicWriteFile(path, data, 0644)
	})
}

func LoadTasksForRepo(repo *config.RepoContext) ([]Task, error) {
	board, err := LoadBoardForRepo(repo)
	if err != nil {
		return nil, err
	}
	return board.Tasks, nil
}

func AddTaskForRepoWithStatus(repo *config.RepoContext, title, status string) (Task, error) {
	path, err := tasksPath(repo)
	if err != nil {
		return Task{}, err
	}
	var t Task
	err = config.WithFileLock(path, func() error {
		board, loadErr := LoadBoardForRepo(repo)
		if loadErr != nil {
			return loadErr
		}
		t = board.AddTask(title, status)
		// Save directly (already under lock).
		board.UpdatedAt = time.Now()
		data, marshalErr := json.MarshalIndent(board, "", "  ")
		if marshalErr != nil {
			return fmt.Errorf("failed to marshal board: %w", marshalErr)
		}
		return config.AtomicWriteFile(path, data, 0644)
	})
	return t, err
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

// AddAndLinkTaskForRepo adds a new task and links it to an instance in a single locked operation.
func AddAndLinkTaskForRepo(repo *config.RepoContext, title, status, instanceTitle string) error {
	return updateBoardForRepo(repo, func(b *Board) error {
		t := b.AddTask(title, status)
		return b.LinkTask(t.ID, instanceTitle)
	})
}

// MoveLinkedTaskForRepo finds a task linked to the given instance, unlinks it,
// and moves it to the given status. Does nothing if no task is linked.
func MoveLinkedTaskForRepo(repo *config.RepoContext, instanceTitle, newStatus string) error {
	return updateBoardForRepo(repo, func(b *Board) error {
		t := b.FindTaskByInstance(instanceTitle)
		if t == nil {
			return nil // nothing to move
		}
		b.UnlinkTask(t.ID)
		return b.MoveTask(t.ID, newStatus)
	})
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
