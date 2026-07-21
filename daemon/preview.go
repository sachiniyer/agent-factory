package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/terminal"
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
	// Modes and HasModes travel with the captured target so preview input can use
	// the same ownership decision as the live stream. HasModes distinguishes an
	// authoritative primary-screen/no-mouse observation from an older or
	// incapable runtime that supplied no mode data.
	Modes    terminal.Modes `json:"terminal_modes,omitempty"`
	HasModes bool           `json:"has_terminal_modes,omitempty"`
	// TabGone NARROWS Gone: the session is alive and well, but the TAB the request
	// addressed is not there — an id that no longer resolves, or an ordinal that is
	// not a slot. Gone is always set alongside it, so a client that only knows
	// about Gone (the TUI's session-gone fallback) behaves exactly as before.
	//
	// It exists because the two causes are indistinguishable to a CLIENT and the
	// daemon knows them apart. `af sessions preview --tab-id x` used to report a
	// tab-level miss as "session %q is no longer running": a plain lie about a
	// running session, and one the CLI could only have avoided by GUESSING from
	// the fact that it had sent a selector — which is wrong precisely when the
	// session really did die mid-capture. Carrying the fact is the alternative to
	// inferring it.
	TabGone bool `json:"tab_gone,omitempty"`
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
	// Resolve the selector once, then carry the selected tab's stable identity all
	// the way through AgentServer.PreviewByID. The precedence — id, then name, then
	// ordinal — is session.ResolveTabIndex's, shared with every other tab verb;
	// the resulting ordinal is used only to read the ID from this SAME snapshot.
	// It is never applied to the live roster later (#2200).
	//
	// A non-empty id that no longer resolves is REFUSED as gone, NOT fallen back to
	// req.Tab (#1779): the fallback previewed whatever tab had shifted into the
	// stale ordinal, precisely the misroute the stable id exists to prevent. Gone
	// (rather than an error) is the honest answer and the shape the TUI already
	// degrades on — the addressed tab is genuinely no longer there.
	//
	// The ordinal is used ONLY when neither an id nor a name was supplied — a
	// legacy client, for which positional addressing is all there ever was. It goes
	// through this same call so it is BOUNDS-CHECKED like every other selector: an
	// in-range ordinal resolves to itself and behaves byte-for-byte as it always
	// did, while an out-of-range one used to reach as.Preview and come back as a
	// silent empty capture with no error at all. A capture verb that answers "" to
	// a bad slot is the dishonesty this cluster exists to remove, so it is now a
	// tab-level Gone like any other missing tab.
	tabs := instance.GetTabs()
	tab, rerr := session.ResolveTabIndex(tabs, req.TabID, req.TabName, req.Tab)
	switch {
	case errors.Is(rerr, session.ErrTabIDNotFound), errors.Is(rerr, session.ErrTabIndexOutOfRange):
		// Gone, not an error: the tab this client addressed is no longer there,
		// which is the fallback the TUI already degrades on. TabGone says the
		// SESSION is fine, so a client need not guess which vanished.
		resp.Gone, resp.TabGone = true, true
		return nil
	case errors.Is(rerr, session.ErrTabNameNotFound):
		// A name that matches nothing is far more likely a typo than a
		// disappearance, so answer with the roster rather than an empty capture.
		ids := make([]string, 0, len(tabs))
		for _, t := range tabs {
			ids = append(ids, session.TabIdentifiers(t))
		}
		return fmt.Errorf("session %q has no tab named %q; its tabs are: %s",
			req.Title, req.TabName, strings.Join(ids, ", "))
	case rerr != nil:
		return rerr
	}
	tabID := tabs[tab].ID
	if tabID == "" {
		return fmt.Errorf("session %q tab %d has no stable identity; reload the session before previewing it", req.Title, tab)
	}
	snapshot, err := as.PreviewByID(tabID, req.Full)
	if err != nil {
		if errors.Is(err, session.ErrTabGone) {
			resp.Gone, resp.TabGone = true, true
			return nil
		}
		if errors.Is(err, tmux.ErrSessionGone) {
			resp.Gone = true
			return nil
		}
		return err
	}
	resp.Content = snapshot.Content
	resp.Modes = snapshot.Modes
	resp.HasModes = snapshot.HasModes
	return nil
}
