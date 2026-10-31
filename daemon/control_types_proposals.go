package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/proposal"
)

// The proposal routes (docs/dev/proposals/proposal-mode/api.md). A request that
// is not session-only names its project with exactly one of SessionID, RepoID or
// RepoPath; RepoPath is a path on the daemon host.

type ListProposalsRequest struct {
	SessionID string `json:"session_id,omitempty"`
	RepoID    string `json:"repo_id,omitempty"`
	RepoPath  string `json:"repo_path,omitempty"`
}

type ListProposalsResponse struct {
	Proposals []proposal.Summary `json:"proposals"`
}

type GetProposalRequest struct {
	SessionID string `json:"session_id,omitempty"`
	RepoID    string `json:"repo_id,omitempty"`
	RepoPath  string `json:"repo_path,omitempty"`
	Slug      string `json:"slug"`
}

type GetProposalResponse = proposal.Detail

type StartProposalRequest struct {
	SessionID string `json:"session_id"`
	Slug      string `json:"slug"`
	// Docs lists the doc names to create; empty means the built-in templates.
	Docs []string `json:"docs,omitempty"`
}

type StartProposalResponse = proposal.Summary

type AttachProposalRequest struct {
	SessionID string `json:"session_id"`
	// Slug is the proposal to attach to; empty reports the current one.
	Slug string `json:"slug,omitempty"`
}

type AttachProposalResponse = proposal.Summary

type DetachProposalRequest struct {
	SessionID string `json:"session_id"`
}

type DetachProposalResponse = proposal.DetachResult

type AddProposalDocRequest struct {
	SessionID string `json:"session_id,omitempty"`
	RepoID    string `json:"repo_id,omitempty"`
	RepoPath  string `json:"repo_path,omitempty"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	// Author is human, agent or external; agent needs SessionID.
	Author string `json:"author"`
}

type AddProposalDocResponse = proposal.DocResult

type RemoveProposalDocRequest struct {
	SessionID string `json:"session_id,omitempty"`
	RepoID    string `json:"repo_id,omitempty"`
	RepoPath  string `json:"repo_path,omitempty"`
	Slug      string `json:"slug"`
	Doc       string `json:"doc"`
	// Author is human, agent or external; agent needs SessionID.
	Author string `json:"author"`
}

type RemoveProposalDocResponse = proposal.DocResult

type ReadProposalDocRequest struct {
	SessionID string `json:"session_id,omitempty"`
	RepoID    string `json:"repo_id,omitempty"`
	RepoPath  string `json:"repo_path,omitempty"`
	Slug      string `json:"slug"`
	Doc       string `json:"doc"`
}

type ReadProposalDocResponse = proposal.ReadResult

type SaveProposalDocRequest struct {
	SessionID string `json:"session_id,omitempty"`
	RepoID    string `json:"repo_id,omitempty"`
	RepoPath  string `json:"repo_path,omitempty"`
	Slug      string `json:"slug"`
	Doc       string `json:"doc"`
	Text      string `json:"text"`
	// Base is the commit of the last read or save of this doc.
	Base string `json:"base"`
	// Writer names the editor that sent the save (a Docs tab's sync) and Seq counts its saves
	// upward. A save whose Seq is not above the highest the daemon has seen from that Writer
	// for the doc is refused (409, proposal_stale_save): a request that outlived its client
	// must not land after the writer's later saves. Both omitted: no fence.
	Writer string `json:"writer,omitempty"`
	Seq    int64  `json:"seq,omitempty"`
}

type SaveProposalDocResponse = proposal.SaveDocResult

type WriteProposalDocRequest struct {
	SessionID string `json:"session_id,omitempty"`
	RepoID    string `json:"repo_id,omitempty"`
	RepoPath  string `json:"repo_path,omitempty"`
	Slug      string `json:"slug"`
	Doc       string `json:"doc"`
	Text      string `json:"text"`
	// Author is human, agent or external; agent needs SessionID.
	Author string `json:"author"`
}

type WriteProposalDocResponse = proposal.DocResult

type PullProposalChangesRequest struct {
	SessionID string `json:"session_id"`
}

type PullProposalChangesResponse = proposal.Changes

type CheckpointProposalRequest struct {
	SessionID string `json:"session_id"`
}

type CheckpointProposalResponse = proposal.Checkpoint

type SetProposalPushModeRequest struct {
	SessionID string `json:"session_id"`
	// Mode is auto or manual.
	Mode string `json:"mode"`
}

type SetProposalPushModeResponse = proposal.PushStatus

type HoldProposalPushRequest struct {
	SessionID string `json:"session_id"`
}

type HoldProposalPushResponse = proposal.PushStatus

type SendProposalChangesRequest struct {
	SessionID string `json:"session_id"`
}

type SendProposalChangesResponse = proposal.PushStatus

func errUnknownProposalMethod(method string) error {
	return fmt.Errorf("internal: %q is not a proposal route", method)
}
