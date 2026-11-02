package backlog

import (
	"fmt"
	"time"
)

type OpenOrchestratorRequest struct {
	RepoID   string `json:"repo_id,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	ID       int    `json:"id"`
}

// What opening a group's orchestrator did before it named the session.
const (
	OrchOpenRestored = "restored"
	OrchOpenReplaced = "replaced"
	OrchOpenStarted  = "started"
)

// OpenOrchestratorResponse names the session to open. Change is
// OrchOpenRestored, OrchOpenReplaced or OrchOpenStarted, or "" when the
// session was live.
type OpenOrchestratorResponse struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Change    string `json:"change,omitempty"`
}

// OrchestratorToOpen is group id's orchestrator, or nil when it has none. It
// refuses an id that is not a group, and a group with none that is archived.
func (b *Backlog) OrchestratorToOpen(id int) (*Orchestrator, error) {
	it, err := b.get(id)
	if err != nil {
		return nil, err
	}
	if !b.isGroup(id) {
		return nil, fmt.Errorf("%s is not a top-level parent; only groups can have an orchestrator", FormatID(id))
	}
	if it.Orchestrator == nil && it.Archived {
		return nil, fmt.Errorf("%s is archived; unarchive it to start an effort session", FormatID(id))
	}
	return it.Orchestrator.clone(), nil
}

// BindNewOrchestrator binds o to group id, which must have no orchestrator.
func (b *Backlog) BindNewOrchestrator(id int, o Orchestrator, now time.Time) error {
	if _, err := b.OrchestratorToOpen(id); err != nil {
		return err
	}
	if prev := b.Item(id).Orchestrator; prev != nil {
		return fmt.Errorf("session %q started orchestrating %s meanwhile; open it instead", prev.SessionID, FormatID(id))
	}
	return b.BindOrchestrator(id, o, now)
}
