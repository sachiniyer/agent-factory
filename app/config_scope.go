package app

import (
	"fmt"
	"path/filepath"
	"sort"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// The config editor's SCOPE picker (`p` inside the `,` overlay).
//
// `af config list --repo <path>` reads the project-effective stack on the CLI;
// this picker is how the TUI reaches the same read — the ledger row
// config.read-project. The pane stays a single writer over a single scope, so a
// scoped read is INSPECTION only: project-scoped writes are the separate
// config.write-project seam, and the pane refuses them rather than writing a
// value it never displayed.
//
// The choices come from the SAME daemon the editor reads: locally that is the
// in-process registry + session roots + root_agents opt-ins + the active
// project (the union buildProjectList makes); on a --daemon-url target it is
// the remote daemon's registry and session snapshot — never the local files,
// for the same observation-and-mutation-must-share-a-target reason as
// #3708/#3626.

// configScopeChoice is one picker row: a label for the overlay and the repo
// selector ReadConfigForEditor takes ("" = the global scope).
type configScopeChoice struct {
	label string
	root  string
}

// showConfigScopePicker opens the scope picker over the still-open config
// editor. The read it feeds is synchronous on the event loop exactly like
// showProjectPickerOverlay's snapshot fetch — a picker that needs a round trip
// before it can list anything shows nothing until the answer arrives.
func (m *home) showConfigScopePicker() (tea.Model, tea.Cmd) {
	choices, err := m.configScopeChoices()
	if err != nil {
		return m, m.handleError(fmt.Errorf("cannot list projects for the config scope: %w", err))
	}
	m.configScopePickerChoices = choices

	items := make([]string, len(choices))
	selected := 0
	for i, choice := range choices {
		items[i] = choice.label
		if choice.root == m.configPane.ScopeRoot() {
			selected = i
		}
	}
	m.selectionOverlay = overlay.NewSelectionOverlay("Config scope", items)
	m.selectionOverlay.SetSelectedIndex(selected)
	m.layoutSelectionOverlay()
	m.state = stateConfigScope
	return m, nil
}

// configScopeChoices builds the picker's rows: the global scope first, then
// every project af knows, sorted by root. The project set is observational —
// it lists what the daemon reports, so a registered-but-sessionless project is
// offered and a project whose path no longer resolves still appears (submitting
// it surfaces the resolver's own error rather than hiding the row).
func (m *home) configScopeChoices() ([]configScopeChoice, error) {
	roots := map[string]bool{}

	if isRemoteTarget() {
		// Registry and sessions BOTH come from the remote daemon: reading the
		// local registry would offer projects the read cannot reach — the picker
		// would name a path that resolves on a different machine.
		var projects []config.Project
		err := withDaemonHTTP(func(c *apiclient.Client) error {
			var e error
			projects, e = c.ListProjects()
			return e
		})
		if err != nil {
			return nil, err
		}
		for _, p := range projects {
			if p.Root != "" {
				roots[p.Root] = true
			}
		}
	} else {
		projects, err := config.ListProjects()
		if err != nil {
			return nil, fmt.Errorf("read project registry: %w", err)
		}
		for _, p := range projects {
			if p.Root != "" {
				roots[p.Root] = true
			}
		}
		if m.appConfig != nil {
			// root_agents opt-ins kept for repos registered before the add-verb
			// rewire — the same union the project switcher keeps (#2456).
			for path := range m.appConfig.RootAgents {
				roots[config.ExpandTilde(path)] = true
			}
		}
		if m.repoRoot != "" {
			roots[m.repoRoot] = true
		}
	}

	// Sessions are the remote-aware source for both targets: the snapshot goes
	// through withDaemonHTTP, which already dials the targeted daemon. A failed
	// snapshot degrades to the registry-only list rather than failing the
	// picker — a scope read of a registered project still works.
	data, err := allReposSnapshotFetcher()
	if err != nil {
		log.WarningLog.Printf("config scope picker: failed to list cross-repo sessions: %v", err)
		data = nil
	}
	for _, d := range data {
		if !session.IsArchivedData(d) && d.Worktree.RepoPath != "" {
			roots[d.Worktree.RepoPath] = true
		}
	}

	sorted := make([]string, 0, len(roots))
	for root := range roots {
		sorted = append(sorted, root)
	}
	sort.Strings(sorted)

	choices := make([]configScopeChoice, 0, len(sorted)+1)
	choices = append(choices, configScopeChoice{label: "Global configuration"})
	for _, root := range sorted {
		choices = append(choices, configScopeChoice{
			label: fmt.Sprintf("%s — %s", filepath.Base(root), root),
			root:  root,
		})
	}
	return choices, nil
}

// handleStateConfigScope routes keys while the scope picker is open. Submit
// re-reads the editor for the chosen scope and returns to it; Esc just closes
// the picker, leaving the pane exactly as it was.
func (m *home) handleStateConfigScope(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.selectionOverlay == nil {
		m.configScopePickerChoices = nil
		m.state = stateConfigEditor
		return m, nil
	}
	if !m.selectionOverlay.HandleKeyPress(msg) {
		return m, nil
	}

	submitted := m.selectionOverlay.IsSubmitted()
	idx := m.selectionOverlay.GetSelectedIndex()
	choices := m.configScopePickerChoices

	m.selectionOverlay = nil
	m.configScopePickerChoices = nil
	m.state = stateConfigEditor

	if !submitted || idx < 0 || idx >= len(choices) {
		return m, nil
	}

	// Read through the same seam the editor opens with, so local and remote
	// targets resolve identically — and so a stale path is an error here, not a
	// global read wearing a project label.
	entries, location, projectRoot, err := ui.ReadConfigForEditor(choices[idx].root)
	if err != nil {
		return m, m.handleError(err)
	}
	m.configPane.SetEntries(entries, location, projectRoot)
	return m, nil
}
