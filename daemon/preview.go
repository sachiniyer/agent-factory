package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
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
	// Tab. When set it wins over Tab, so a capture can't grab the wrong tab after a
	// reorder/close.
	//
	// A TabID that no longer resolves answers Gone=true — it does NOT fall back to
	// the ordinal Tab, which is what the handler actually does (#1779) and the
	// opposite of what this comment claimed until #1904. The fallback previewed
	// whatever tab had shifted into the stale ordinal: the exact misroute the id
	// exists to prevent, wearing a backward-compatible face. The ordinal is used
	// ONLY when no id was supplied at all — a legacy client that never had one.
	//
	// This is the repo-wide rule for every tab verb, stated in
	// tab_id_addressing_test.go and shared by Rename/Reorder/CloseTab's
	// resolveTabTarget (#1929): once a client addresses a tab by its stable id, no
	// daemon path may fall back to a positional one.
	TabID string `json:"tab_id,omitempty"`
	// TabName addresses the tab by its canonical name — the handle a user types
	// (#1986), the one `af sessions tab-create` printed. It is what a person has
	// on hand: `af sessions preview alpha --tab-name shell` (#1948). Resolved
	// against the roster the DAEMON holds, so the CLI never has to fetch the tab
	// list and race it.
	//
	// Ranks BELOW TabID and ABOVE Tab, the precedence every tab verb shares (see
	// session.ResolveTabIndex). A name that matches nothing is an ERROR listing the
	// tabs that exist — unlike an unresolvable TabID, which answers Gone: an id
	// that stops resolving means the exact tab the client was looking at went
	// away, while a name that matches nothing is far more likely a typo, and a
	// typo deserves the roster rather than a silent empty capture.
	TabName string `json:"tab_name,omitempty"`
	Full    bool   `json:"full"`
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
	// Address the capture by the stable tab id (#1738), or by name (#1948), when
	// the client gives one: resolve it to the tab's CURRENT ordinal so a capture
	// follows the tab across a reorder/close. The precedence — id, then name, then
	// ordinal — is session.ResolveTabIndex's, shared with every other tab verb
	// rather than restated here.
	//
	// A non-empty id that no longer resolves is REFUSED as gone, NOT fallen back to
	// req.Tab (#1779): the fallback previewed whatever tab had shifted into the
	// stale ordinal, precisely the misroute the stable id exists to prevent. Gone
	// (rather than an error) is the honest answer and the shape the TUI already
	// degrades on — the addressed tab is genuinely no longer there.
	//
	// The ordinal is used ONLY when neither an id nor a name was supplied — a
	// legacy client, for which positional addressing is all there ever was — and
	// that path is left byte-for-byte as it was, so such a client is unaffected.
	tab := req.Tab
	if req.TabID != "" || req.TabName != "" {
		idx, rerr := session.ResolveTabIndex(instance.GetTabs(), req.TabID, req.TabName, req.Tab)
		switch {
		case errors.Is(rerr, session.ErrTabIDNotFound):
			// Gone, not an error: the specific tab this client was looking at is no
			// longer there, which is the fallback the TUI already degrades on.
			resp.Gone = true
			return nil
		case errors.Is(rerr, session.ErrTabNameNotFound):
			// A name that matches nothing is far more likely a typo than a
			// disappearance, so answer with the roster rather than an empty capture.
			tabs := instance.GetTabs()
			ids := make([]string, 0, len(tabs))
			for _, t := range tabs {
				ids = append(ids, session.TabIdentifiers(t))
			}
			return fmt.Errorf("session %q has no tab named %q; its tabs are: %s",
				req.Title, req.TabName, strings.Join(ids, ", "))
		case rerr != nil:
			return rerr
		}
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
