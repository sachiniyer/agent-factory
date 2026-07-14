package daemon

import (
	"errors"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The Preview RPC (#1592 Phase 2 PR6) is the daemon's SOLE capture path for the
// session content the TUI cannot stream live over the WS PTY plane: remote/hook
// sessions (whose output is captured by the daemon-side hook process, not the
// TUI), scroll-mode scrollback (the live stream carries only the visible screen),
// the transient #1321 preview target, and any transitional state. Before PR6 the
// TUI shelled out to `tmux capture-pane` itself; deleting that made the daemon the
// one capturer, and this RPC is how the read-only TUI reaches it.
//
// It is an internal, non-cataloged route (like the Pause/ResumeStatusPoll attach-
// coordination RPCs): it is TUI render plumbing, not a client-facing `af` verb, so
// it stays out of the public `af api` surface.

// PreviewRequest asks the daemon to capture tab `Tab`'s content for a session
// (Title, optional RepoID). Full=true returns the entire scrollback history (the
// scroll-mode source); false returns the visible screen. Tab 0 is the agent tab;
// Tab>0 is a shell/process tab.
type PreviewRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	Tab    int    `json:"tab"`
	// TabID addresses the tab by its stable id (#1738) rather than its ordinal
	// Tab. When set and it resolves against the session's live tab list it wins
	// over Tab, so a capture can't grab the wrong tab after a reorder/close; when
	// empty (or it no longer resolves) the handler falls back to the ordinal Tab
	// for backward compatibility.
	TabID string `json:"tab_id,omitempty"`
	Full  bool   `json:"full"`
}

// PreviewResponse carries the captured content, or Gone=true when the session's
// tmux vanished mid-capture. Gone is surfaced as a structured field rather than an
// error string so the TUI can map it back to the exact fallback it always showed
// on tmux.ErrSessionGone (the sentinel doesn't survive the HTTP boundary).
type PreviewResponse struct {
	Content string `json:"content"`
	Gone    bool   `json:"gone,omitempty"`
}

// Preview captures one tab's content through the session's agent-server — the same
// tab-aware Preview the WS broker's runtime exposes. A vanished tmux session is
// reported as Gone rather than an error so the read-only TUI degrades to its
// session-gone fallback without parsing error strings.
func (s *controlServer) Preview(req PreviewRequest, resp *PreviewResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	as, instance, err := s.manager.agentServerForStream(req.Title, req.RepoID)
	if err != nil {
		return err
	}
	// Prefer the stable tab id (#1738): resolve it to the tab's CURRENT ordinal so
	// a capture addresses the right tab after a reorder/close. Fall back to the
	// ordinal Tab when no id is given or it no longer resolves (legacy clients /
	// a since-closed tab).
	tab := req.Tab
	if idx, ok := instance.TabIndexByID(req.TabID); ok {
		tab = idx
	}
	content, err := as.Preview(tab, req.Full)
	if err != nil {
		if errors.Is(err, tmux.ErrSessionGone) {
			resp.Gone = true
			return nil
		}
		return err
	}
	resp.Content = content
	return nil
}
