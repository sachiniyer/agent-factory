package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// handleRebindProject dispatches the picker's `b` verb: rebind a registered
// project's stable id to the checkout at the entered path. The user-typed path
// is resolved against the TUI's filesystem — this process shares the user's
// machine, so ResolveUserPath is correct here exactly as in handleAddProject —
// and the daemon performs the registry write off the event loop, mirroring
// addProjectCmd. The picker stays open until the answer lands: a rejection
// (not a git repo, path owned by another project) is corrected inline via
// SetRebindError, a success closes and refreshes.
func (m *home) handleRebindProject(req overlay.RebindRequest) (tea.Model, tea.Cmd) {
	abs, err := config.ResolveUserPath(req.Path)
	if err != nil {
		m.projectPickerOverlay.SetRebindError(fmt.Sprintf("cannot resolve path: %s", req.Path))
		return m, nil
	}
	req.Path = abs
	return m, m.rebindProjectCmd(req)
}

// rebindProjectCmd rebinds a registered project through the daemon — the single
// writer (#960) — off the event loop, mirroring addProjectCmd/deleteProjectCmd.
// The reply carries the request's token so handleProjectRebound can tell it
// apart from a reply to any other picker.
func (m *home) rebindProjectCmd(req overlay.RebindRequest) tea.Cmd {
	return func() tea.Msg {
		project, err := rebindProjectThroughDaemon(req.Project.RegistryID, req.Path)
		root := project.Root
		if root == "" {
			root = req.Path
		}
		return projectReboundMsg{
			token:           req.Token,
			projectID:       req.Project.RegistryID,
			oldRepoID:       req.Project.RepoID,
			oldRoot:         req.Project.Root,
			oldRegistryRoot: req.Project.RegistryRoot,
			name:            req.Project.Name,
			root:            root,
			err:             err,
		}
	}
}

// handleProjectRebound finalizes an async rebind. The reply belongs to the
// picker only when that picker is still open AND still waiting on this
// request's token; a picker closed and reopened in the meantime is a different
// request's (or none's), and must neither close nor show this reply's error.
//
// Three outcomes:
//   - Confirmed (success, or a committed error whose follow-up step failed): the
//     picker it owns closes, the Projects section is re-read, and a move of the
//     project the TUI is scoped to is followed to wherever the registry binds it
//     now (followActiveRebind). The success toast shows only when no other
//     picker is on screen — over a different picker it reads as that picker's.
//   - Unknown (lost in transport, cancelled): the picker it owns closes, the
//     Projects section is re-read, the scope is left alone, and one notice says
//     the outcome is unknown. Nothing is followed: the refreshed list is where
//     the user sees what happened.
//   - A definitive refusal: fed back inline to the picker that owns it, which
//     re-arms. An unowned one changed nothing; over a different picker it is
//     logged rather than shown, else it goes to the error box.
func (m *home) handleProjectRebound(msg projectReboundMsg) (tea.Model, tea.Cmd) {
	pickerOpen := m.projectPickerOverlay != nil && m.state == stateSwitchProject
	owned := pickerOpen && m.projectPickerOverlay.OwnsRebindReply(msg.token)
	committed := msg.err != nil && apiproto.IsMutationCommitted(msg.err)
	if msg.err != nil && !committed {
		if !rebindOutcomeUnknown(msg.err) {
			if owned {
				m.projectPickerOverlay.SetRebindError(msg.err.Error())
				return m, nil
			}
			if pickerOpen {
				// A definitive refusal changed nothing, and shown over a
				// different picker it reads as THAT picker's result — the same
				// reason a stale success raises no toast there. Log it instead.
				log.WarningLog.Printf("stale rebind of project %q refused while another picker is open: %v", msg.name, msg.err)
				return m, nil
			}
			return m, m.handleError(fmt.Errorf("failed to rebind project %q: %w", msg.name, msg.err))
		}
		if owned {
			m.closeProjectPicker()
		}
		m.refreshSidebarProjects()
		return m, m.handleError(fmt.Errorf("Rebind of %s · outcome unknown · check the project list", msg.name))
	}
	m.refreshSidebarProjects()
	if owned {
		m.closeProjectPicker()
	}
	var notice tea.Cmd
	switch {
	case committed:
		// The move landed; a follow-up step failed. Follow it, and say what failed.
		notice = m.handleError(fmt.Errorf("rebound project %q, but: %w", msg.name, msg.err))
	case owned || !pickerOpen:
		// Name the root the registry holds NOW, never this reply's echo: another
		// client may have rebound the project again since this request committed.
		// A record that is gone (or unreadable) gets a root-free message.
		text := fmt.Sprintf("Rebound project '%s'", msg.name)
		if root, ok := registeredProjectRoot(msg.projectID); ok {
			text = fmt.Sprintf("Rebound project '%s' to %s", msg.name, root)
		}
		notice = m.showTransientMessage(text)
	}
	model, followCmd := m.followActiveRebind(msg)
	return model, tea.Batch(notice, followCmd)
}

// rebindOutcomeUnknown reports whether a failed rebind's outcome is not known:
// the reply was lost in transport (the daemon may have written the registry
// and then exited), or the request was cancelled mid-flight. An error the
// daemon returned in its envelope is a definitive refusal, and a committed
// error (apiproto.IsMutationCommitted) is a known, landed move — neither is
// unknown.
func rebindOutcomeUnknown(err error) bool {
	return apiclient.IsTransportError(err) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

// followActiveRebind moves the TUI's scope after a CONFIRMED rebind of the
// project it is scoped to. It follows the registry, never the reply's echo:
// another client may have rebound or deleted the project since this request
// committed, and the Projects section just re-read that newer state. A record
// that is gone, a registry that cannot be read, or a record still naming the
// root it recorded (a no-op rebind, or one another client moved back) leaves
// the scope where it is. Unknown outcomes never reach it.
func (m *home) followActiveRebind(msg projectReboundMsg) (tea.Model, tea.Cmd) {
	if !m.rebindMovedActiveProject(msg) {
		return m, nil
	}
	root, ok := registeredProjectRoot(msg.projectID)
	// "Did the record move" compares registry to registry: the recorded root
	// when the rebind was sent against the one now. The row's display root can
	// come from another source, and an unmoved record would then look moved.
	recorded := msg.oldRegistryRoot
	if recorded == "" {
		recorded = msg.oldRoot
	}
	if !ok || samePath(root, recorded) || samePath(root, m.repoRoot) {
		return m, nil
	}
	return m.switchToProjectRoot(root)
}

// samePath compares two roots under the #2110 spelling rule; an empty side
// never matches.
func samePath(a, b string) bool {
	return a != "" && b != "" &&
		pathutil.ResolveForCompare(filepath.Clean(a)) == pathutil.ResolveForCompare(filepath.Clean(b))
}

// registeredProjectRoot returns the root the registry binds project id to now,
// or false when no readable record has that id.
func registeredProjectRoot(id string) (string, bool) {
	projects, err := config.ListProjects()
	if err != nil {
		return "", false
	}
	for _, p := range projects {
		if p.ID == id {
			return p.Root, true
		}
	}
	return "", false
}

// rebindMovedActiveProject reports whether a rebind moved the project the TUI
// is scoped to: the row's aggregation identity is the active repoID, or its
// root is the active root.
func (m *home) rebindMovedActiveProject(msg projectReboundMsg) bool {
	if msg.oldRepoID != "" && msg.oldRepoID == m.repoID {
		return true
	}
	return samePath(msg.oldRoot, m.repoRoot)
}
