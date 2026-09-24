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
			token:     req.Token,
			projectID: req.Project.RegistryID,
			oldRepoID: req.Project.RepoID,
			oldRoot:   req.Project.Root,
			name:      req.Project.Name,
			root:      root,
			err:       err,
		}
	}
}

// handleProjectRebound finalizes an async rebind. The reply belongs to the
// picker only when that picker is still open AND still waiting on this
// request's token; a picker closed and reopened in the meantime is a different
// request's (or none's), and must neither close nor show this reply's error.
//
// A definitive daemon refusal it owns is fed back inline so the user can correct
// the path. Any outcome that may have landed — success, a committed error, or an
// uncertain one — never re-arms the form: the picker it owns closes, and the
// Projects section is re-read so it shows where the registration points now. A
// success announces itself with a toast only when no other picker is on screen,
// since a toast over a different picker reads as that picker's result; an
// unknown outcome always says so. A success that moved the project the TUI is
// scoped to switches to wherever the registry binds that project now, so new
// sessions and tasks stop targeting the root that was just repaired away.
func (m *home) handleProjectRebound(msg projectReboundMsg) (tea.Model, tea.Cmd) {
	pickerOpen := m.projectPickerOverlay != nil && m.state == stateSwitchProject
	owned := pickerOpen && m.projectPickerOverlay.OwnsRebindReply(msg.token)
	if msg.err != nil {
		if !rebindOutcomeUncertain(msg.err) {
			if owned {
				m.projectPickerOverlay.SetRebindError(msg.err.Error())
				return m, nil
			}
			return m, m.handleError(fmt.Errorf("failed to rebind project %q: %w", msg.name, msg.err))
		}
		if owned {
			m.closeProjectPicker()
		}
		m.refreshSidebarProjects()
		return m, m.handleError(fmt.Errorf("rebind of project %q could not be confirmed — check the Projects list before retrying: %w", msg.name, msg.err))
	}
	m.refreshSidebarProjects()
	var toast tea.Cmd
	if owned {
		m.closeProjectPicker()
	}
	if owned || !pickerOpen {
		toast = m.showTransientMessage(fmt.Sprintf("Rebound project '%s' to %s", msg.name, msg.root))
	}
	if m.rebindMovedActiveProject(msg) {
		// Follow the registry, not this reply's echo: another client may have
		// rebound or deleted the project since this request committed, and the
		// Projects section just re-read that newer state. A record that is gone
		// (or a registry that cannot be read) leaves the scope where it is.
		if root, ok := registeredProjectRoot(msg.projectID); ok {
			model, switchCmd := m.switchToProjectRoot(root)
			return model, tea.Batch(toast, switchCmd)
		}
	}
	return m, toast
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

// rebindOutcomeUncertain reports whether a failed rebind may nonetheless have
// landed: the daemon said it committed before a follow-up failed, the reply was
// lost in transport (the daemon may have written the registry and then exited),
// or the request was cancelled mid-flight. Only an error the daemon returned in
// its envelope is a definitive refusal that is safe to correct and resubmit.
func rebindOutcomeUncertain(err error) bool {
	return apiproto.IsMutationCommitted(err) ||
		apiclient.IsTransportError(err) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

// rebindMovedActiveProject reports whether a rebind moved the project the TUI
// is scoped to: the row's aggregation identity is the active repoID, or its
// root is the active root.
func (m *home) rebindMovedActiveProject(msg projectReboundMsg) bool {
	if msg.oldRepoID != "" && msg.oldRepoID == m.repoID {
		return true
	}
	return msg.oldRoot != "" && m.repoRoot != "" &&
		pathutil.ResolveForCompare(filepath.Clean(msg.oldRoot)) == pathutil.ResolveForCompare(filepath.Clean(m.repoRoot))
}
