package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/backlog"
)

// The backlog routes (backlog proposal, api.md). A project request names its
// project with exactly one of RepoID or RepoPath; RepoPath is a path on the
// daemon host.

type (
	GetBacklogRequest            = backlog.GetRequest
	GetBacklogResponse           = backlog.View
	AddBacklogItemsRequest       = backlog.AddRequest
	AddBacklogItemsResponse      = backlog.AddResponse
	UpdateBacklogItemRequest     = backlog.UpdateItemRequest
	UpdateBacklogItemResponse    = backlog.ItemResponse
	MoveBacklogItemRequest       = backlog.MoveRequest
	MoveBacklogItemResponse      = backlog.Empty
	SetBacklogItemStatusRequest  = backlog.SetItemStatusRequest
	SetBacklogItemStatusResponse = backlog.ItemResponse
	DeleteBacklogItemRequest     = backlog.DeleteItemRequest
	DeleteBacklogItemResponse    = backlog.Empty
	PostBacklogMessageRequest    = backlog.PostMessageRequest
	PostBacklogMessageResponse   = backlog.MessageResponse
	SetBacklogSettingsRequest    = backlog.SetSettingsRequest
	SetBacklogSettingsResponse   = backlog.SettingsResponse
	GetBudgetRequest             = backlog.GetBudgetRequest
	GetBudgetResponse            = backlog.BudgetResponse
	SetBudgetRequest             = backlog.SetBudgetRequest
	SetBudgetResponse            = backlog.BudgetResponse
	StartBacklogItemRequest      = backlog.StartRequest
	StartBacklogItemResponse     = backlog.StartResponse
	RunBacklogRequest            = backlog.RunRequest
	RunBacklogResponse           = backlog.RunResponse
)

func errUnknownBacklogMethod(method string) error {
	return fmt.Errorf("internal: %q is not a backlog route", method)
}
