package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
)

// agentTabCreateRequest asks the workspace to spawn one tab under the daemon's
// STABLE id (#5571 W4.3). Kind is the tabKindVocabulary wire name — "shell" or
// "process" are the only kinds this route creates (web/vscode tabs carry no
// tmux here and are metadata the daemon stages itself; the agent tab is made
// with the session). ID is verbatim: the driving daemon is the id authority, and
// the id doubles as the create's idempotency key — a retry that repeats id+kind
// (and command, for a process tab) answers with the tab the first create made;
// an id collision on a different shape refuses. Empty ID mints a fresh id, for
// callers that do not hold the authority.
type agentTabCreateRequest struct {
	ID      string `json:"id,omitempty"`
	Kind    string `json:"kind"`
	Command string `json:"command,omitempty"`
	Name    string `json:"name,omitempty"`
}

// agentTabCreateResponse returns the created tab as the roster row every
// /v1/agent/tabs caller reads — one shape, so the create answer and the roster
// can never drift.
type agentTabCreateResponse struct {
	Tab session.TabData `json:"tab"`
}

// TabCreate spawns a shell or process tab inside the workspace. It is deliberately
// NOT a second admission path: it delegates to the same Instance.AddShellTabWithID /
// AddProcessTabWithID the daemon's own tab verbs use, so every fence (started,
// archived, account-swap, local-worktree availability) applies identically to a
// remote caller.
func (hs *headlessServer) TabCreate(req agentTabCreateRequest, resp *agentTabCreateResponse) error {
	tabs, err := hs.tabStore()
	if err != nil {
		return err
	}
	var tab *session.Tab
	switch req.Kind {
	case session.TabKindName(session.TabKindShell):
		tab, err = tabs.AddShellTabWithID(req.ID)
	case session.TabKindName(session.TabKindProcess):
		if strings.TrimSpace(req.Command) == "" {
			return fmt.Errorf("a %q tab requires a non-empty command", req.Kind)
		}
		tab, err = tabs.AddProcessTabWithID(req.ID, req.Command, req.Name)
	default:
		return fmt.Errorf("unsupported tab kind %q: tab-create makes %q and %q tabs",
			req.Kind, session.TabKindName(session.TabKindShell), session.TabKindName(session.TabKindProcess))
	}
	if err != nil {
		return err
	}
	// Answer the roster row, not a second projection of the Tab: /v1/agent/tabs
	// and this response share tabDataLocked, so fields like kind_name and the
	// exit record can never drift between them.
	for _, td := range tabs.TabsData() {
		if td.ID == tab.ID {
			resp.Tab = td
			return nil
		}
	}
	return fmt.Errorf("created tab %q is absent from the roster", tab.ID)
}

// agentTabCloseRequest names the tab to close by its stable id — never an
// ordinal: an ordinal close on a remote wire is the misroute the id exists to
// prevent.
type agentTabCloseRequest struct {
	ID string `json:"id"`
	// Gen, when set, makes the close conditional on the tab's incarnation: the
	// match is checked atomically with the removal, so a retried close cannot
	// take down a newer tab that reused the id. A mismatch is a 404 tab_gone.
	Gen string `json:"gen,omitempty"`
}

// genCloser is the conditional close a real *session.Instance serves.
type genCloser interface {
	CloseTabByIDIfGen(tabID, gen string) error
}

// TabClose closes the tab req.ID names through the same CloseTabByID the
// daemon's own close path issues, so its broker teardown and stream ends fire
// identically. A stale or empty id is a 404 through the shared ErrTabGone
// mapping; an empty id fails validation first.
func (hs *headlessServer) TabClose(req agentTabCloseRequest, resp *agentOKResponse) error {
	if req.ID == "" {
		return fmt.Errorf("a stable tab id is required")
	}
	tabs, err := hs.tabStore()
	if err != nil {
		return err
	}
	closeErr := error(nil)
	if req.Gen != "" {
		gc, ok := tabs.(genCloser)
		if !ok {
			return fmt.Errorf("this agent-server cannot close a tab conditionally on its generation")
		}
		closeErr = gc.CloseTabByIDIfGen(req.ID, req.Gen)
	} else {
		closeErr = tabs.CloseTabByID(req.ID)
	}
	if closeErr != nil {
		return closeErr
	}
	resp.OK = true
	return nil
}

// agentTabsResponse is the roster: every tab with its stable id, kind, name,
// and — for process tabs — the exit evidence RecordProcessTabExits just
// refreshed (status, time, stopped_by).
type agentTabsResponse struct {
	Tabs []session.TabData `json:"tabs"`
}

// Tabs answers the workspace's tab roster. It refreshes process-exit evidence
// first: a process tab whose command finished since the last roster read would
// otherwise report a stale un-exited row forever — the restore path stamps this
// on load, but a LIVE sandbox has no load, so the roster read is where a live
// exit gets recorded.
func (hs *headlessServer) Tabs(_ struct{}, resp *agentTabsResponse) error {
	tabs, err := hs.tabStore()
	if err != nil {
		return err
	}
	tabs.RecordProcessTabExits()
	resp.Tabs = tabs.TabsData()
	return nil
}

// tabStore returns the roster the tab routes act on, or refuses when this
// server was built without one (a unit test exercising only the non-tab
// surface). The refusal beats a nil-pointer panic in the handler.
func (hs *headlessServer) tabStore() (tabKeeper, error) {
	if hs.tabs == nil {
		return nil, fmt.Errorf("this agent-server has no tab roster")
	}
	return hs.tabs, nil
}
