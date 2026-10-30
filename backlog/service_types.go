package backlog

import "github.com/sachiniyer/agent-factory/planusage"

// The backlog routes. Each project request names its project with exactly one
// of RepoID or RepoPath; RepoPath is a path on the daemon host.

type GetRequest struct {
	RepoID   string `json:"repo_id,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
}

type View struct {
	Backlog *Backlog        `json:"backlog"`
	States  map[int]Derived `json:"states"`
	Runner  RunnerView      `json:"runner"`
}

const (
	RunnerOff     = "off"
	RunnerIdle    = "idle"
	RunnerWaiting = "waiting"
	RunnerStopped = "stopped"
)

// RunnerView is the runner bar: its state and why, and the slots in use.
type RunnerView struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Used   int    `json:"used"`
	Max    int    `json:"max"`
}

type AddRequest struct {
	RepoID   string      `json:"repo_id,omitempty"`
	RepoPath string      `json:"repo_path,omitempty"`
	Proposal string      `json:"proposal,omitempty"`
	Items    []ItemInput `json:"items"`
	// Before and After place a single new item next to a sibling.
	Before int `json:"before,omitempty"`
	After  int `json:"after,omitempty"`
}

type AddResponse struct {
	IDs   map[string]int `json:"ids"`
	Items []Item         `json:"items"`
}

type UpdateItemRequest struct {
	RepoID     string  `json:"repo_id,omitempty"`
	RepoPath   string  `json:"repo_path,omitempty"`
	ID         int     `json:"id"`
	Title      *string `json:"title,omitempty"`
	Body       *string `json:"body,omitempty"`
	WaitsOn    *[]int  `json:"waits_on,omitempty"`
	Program    *string `json:"program,omitempty"`
	Sequential *bool   `json:"sequential,omitempty"`
}

type ItemResponse struct {
	Item Item `json:"item"`
}

type MoveRequest struct {
	RepoID   string `json:"repo_id,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	ID       int    `json:"id"`
	Before   int    `json:"before,omitempty"`
	After    int    `json:"after,omitempty"`
	Into     int    `json:"into,omitempty"`
	Top      bool   `json:"top,omitempty"`
	Bottom   bool   `json:"bottom,omitempty"`
}

type Empty struct{}

type SetItemStatusRequest struct {
	RepoID   string `json:"repo_id,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	ID       int    `json:"id"`
	Status   Status `json:"status"`
	// SessionID marks the agent of that item session sending it to review.
	SessionID string `json:"session_id,omitempty"`
	PR        string `json:"pr,omitempty"`
}

type DeleteItemRequest struct {
	RepoID   string `json:"repo_id,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	ID       int    `json:"id"`
}

type PostMessageRequest struct {
	RepoID     string `json:"repo_id,omitempty"`
	RepoPath   string `json:"repo_path,omitempty"`
	ID         int    `json:"id"`
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	SessionID  string `json:"session_id,omitempty"`
	QuestionID int    `json:"question_id,omitempty"`
}

type MessageResponse struct {
	Message Message `json:"message"`
}

type SetSettingsRequest struct {
	RepoID     string  `json:"repo_id,omitempty"`
	RepoPath   string  `json:"repo_path,omitempty"`
	Runner     *bool   `json:"runner,omitempty"`
	MaxRunning *int    `json:"max_running,omitempty"`
	OnDone     *string `json:"on_done,omitempty"`
}

type SettingsResponse struct {
	Settings Settings `json:"settings"`
}

type GetBudgetRequest struct{}

type SetBudgetRequest struct {
	Share     *float64 `json:"share,omitempty"`
	StopAt    *float64 `json:"stop_at,omitempty"`
	MaxPerRun *float64 `json:"max_per_run,omitempty"`
	USDLimit  *float64 `json:"usd_limit,omitempty"`
}

// BudgetResponse is the budget with the plan windows: plan % and the part the
// backlog used, per window.
type BudgetResponse struct {
	Budget Budget `json:"budget"`
	planusage.Usage
}

// ChangedEvent is the payload of a backlog.changed event.
type ChangedEvent struct {
	RepoID string `json:"repo_id"`
	IDs    []int  `json:"ids"`
}

type StartRequest struct {
	RepoID   string `json:"repo_id,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	ID       int    `json:"id"`
}

// StartResponse returns the run at once; a new session's run is still
// starting.
type StartResponse struct {
	Run RunRef `json:"run"`
}
