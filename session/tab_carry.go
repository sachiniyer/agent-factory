package session

import (
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// restoreCarriedTabs rebuilds the non-agent tabs of a record a previous instance
// held onto this freshly launched one (#2628).
//
// It exists because the root agent's heal is the ONE recovery path that replaces
// the session record instead of re-spawning into it. Every other Lost session
// keeps its record, so LocalBackend.respawn walks the persisted roster and
// reconnects each tab by its exact tmux name; a root's replacement is a fresh
// create, and a fresh create comes up with only its agent tab (#1100). A root
// that had a terminal, a `tail -f` process tab, a dev-server web tab, or an
// editor tab therefore lost all of them to a tmux outage — visibly, with nothing
// to restore them from once the record holding them was deleted.
//
// Three decisions this encodes, all of them chosen to make the heal look like
// the restore it stands in for rather than like something new:
//
//   - Ids CARRY. These are the same logical tabs — same names, same targets,
//     same order — and the roster is what a client reconciles against by id
//     (ReconcileTabsFromData, the web's tab bindings). Re-minting would present
//     a tab strip of strangers and drop any pane binding pointed at one. The
//     replacement record does get a new SESSION id, so nothing keeps an open
//     stream alive across the heal either way; that is the record swap, not this.
//   - Process tabs RE-RUN their command, because tmux does it: setupTabs restores
//     each tab by name, and TmuxSession.Restore re-spawns a definitively-absent
//     session in the worktree (#386). That is exactly what a Lost restore already
//     does to every other session's process tabs, and a root whose heal alone
//     produced dead rows would be the surprising one.
//   - Tmux names are REUSED verbatim. The replacement root has the same title in
//     the same repo, so it derives the same agent session name and the carried
//     "<agent>__<token>" names still belong to it — and the sessions they name
//     were just killed by the reap, so retaking them is free (the #1957 rule that
//     setupTabs' dead-shell replacement already follows). A name that does not
//     carry the current prefix, or that another carried tab already took, is
//     re-derived instead of trusted.
//
// It only builds tab objects and their tmux HANDLES; nothing is spawned here.
// The caller runs setupTabs immediately after, which restores (and, for a dead
// shell, replaces) each one under the orphan/race discipline that already lives
// there — so this function can never leak a tmux session, and a launch that
// fails afterwards tears down the rebuilt tabs with all the others.
//
// A no-op unless the instance is a fresh single-tab launch carrying a roster, so
// the shared setupTabs call site can invoke it on the restore path too.
func (i *Instance) restoreCarriedTabs() {
	i.mu.Lock()
	carried := i.carriedTabs
	// One-shot: a retried launch of the same object must not rebuild the tabs a
	// previous attempt already appended.
	i.carriedTabs = nil
	agentTmux := i.tmuxLocked()
	existing := append([]*Tab(nil), i.Tabs...)
	title := i.Title
	i.mu.Unlock()

	// len(existing) != 1 means this is not a fresh launch's agent-only roster —
	// a restore, or a second pass — and rebuilding into it would duplicate tabs.
	if len(carried) == 0 || agentTmux == nil || len(existing) != 1 {
		return
	}

	prefix := agentTmux.SanitizedName() + tmuxTabSeparator
	usedNames := map[string]bool{existing[0].Name: true}
	usedTokens := map[string]bool{}
	rebuilt := make([]*Tab, 0, len(carried))

	for idx, td := range carried {
		kind := tabKindForData(td.Kind)
		// Index 0 is the agent tab, which the launch just spawned itself (on the
		// carried conversation, since #2616). The kind check covers a hand-edited
		// or forward-incompatible record listing a second one: an instance has
		// exactly one agent tab, at Tabs[0].
		if idx == 0 || kind == TabKindAgent {
			continue
		}
		if len(existing)+len(rebuilt) >= maxTabs {
			log.WarningLog.Printf("re-created session %q: not restoring carried tab %q — the roster is already at the %d-tab cap", title, td.Name, maxTabs)
			continue
		}

		name := sanitizeTabName(td.Name)
		if name == "" {
			name = carriedTabBaseName(kind, td.Command)
		}
		name = firstFreeName(usedNames, name)
		usedNames[name] = true

		id := td.ID
		if id == "" {
			// A row persisted before stable tab ids (#1738), the same backfill
			// restoreLocalTabs does on load.
			id = newTabID()
		}
		tab := &Tab{ID: id, Name: name, Kind: kind, Command: td.Command, URL: td.URL}
		if kind.HasTmux() {
			token := ""
			if strings.HasPrefix(td.TmuxName, prefix) {
				token = strings.TrimPrefix(td.TmuxName, prefix)
			}
			if token == "" || usedTokens[token] {
				token = firstFreeName(usedTokens, name)
			}
			usedTokens[token] = true
			tab.tmux = agentTmux.NewSiblingSession(prefix+token, tabProgram(kind, td.Command, ""))
		}
		rebuilt = append(rebuilt, tab)
	}
	if len(rebuilt) == 0 {
		return
	}

	i.mu.Lock()
	// Re-confirm the roster is still the agent-only one read above rather than
	// appending blind: nothing else touches a mid-launch instance today (it is
	// not registered with the daemon yet), and this keeps that an assertion
	// instead of an assumption.
	if len(i.Tabs) == 1 {
		i.Tabs = append(i.Tabs, rebuilt...)
	}
	i.mu.Unlock()
}

// carriedTabBaseName names a carried tab whose recorded name is empty or is not
// usable in a tmux session name. Each kind falls back to the same base its own
// Add*Tab path would have used, so a repaired name reads like one af chose.
func carriedTabBaseName(kind TabKind, command string) string {
	switch kind {
	case TabKindWeb:
		return webTabName
	case TabKindVSCode:
		return vscodeTabName
	case TabKindProcess:
		return processTabBaseName("", command)
	default:
		return shellTabName
	}
}
