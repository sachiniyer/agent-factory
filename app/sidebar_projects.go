package app

import (
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// The Projects section's row plumbing: the conversion between the union's
// overlay.Project rows and the pane's SidebarProject rows, and the row sets a
// preserving merge draws on — the registry-backed half a degraded read cannot
// prove absent, minus the rows this session itself just deleted or rebound
// (#7522 review). The refresh/apply funnel that installs the merged set lives
// in switch_project.go beside the picker whose list build it shares.

// projectRows maps the discovered project list into Projects-section rows,
// marking the active project so the section highlights where the rail is scoped.
func (m *home) projectRows(projects []overlay.Project) []ui.SidebarProject {
	rows := make([]ui.SidebarProject, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, ui.SidebarProject{
			RepoID:       p.RepoID,
			CheckoutID:   p.RegistryCheckoutID,
			Name:         p.Name,
			Root:         p.Root,
			SessionCount: p.SessionCount,
			InPlaceCount: p.InPlaceCount,
			Active:       p.Root == m.repoRoot,
		})
	}
	return rows
}

// sidebarProjectRows is projectRows' inverse for the merge fallback in
// applySidebarProjects: a pane row has no registry fields, so the converted row
// is session-derived — the merge preserves it unconditionally, and the coalesce
// grafts a fresh registration's identity back onto it when the RepoID matches.
func sidebarProjectRows(rows []ui.SidebarProject) []overlay.Project {
	projects := make([]overlay.Project, 0, len(rows))
	for _, r := range rows {
		projects = append(projects, overlay.Project{
			RepoID:       r.RepoID,
			Name:         r.Name,
			Root:         r.Root,
			SessionCount: r.SessionCount,
			InPlaceCount: r.InPlaceCount,
		})
	}
	return projects
}

// registryBackedRows filters a row set to the registry-backed half — the rows
// a degraded registry read cannot prove absent, for a merge where the session
// snapshot is live truth and the old session rows would only shadow it.
func registryBackedRows(rows []overlay.Project) []overlay.Project {
	registry := make([]overlay.Project, 0, len(rows))
	for _, r := range rows {
		if r.RegistryID != "" {
			// Both callers run this only when the session snapshot landed and
			// the REGISTRY read degraded, so the snapshot is the authority on
			// counts: a repo it does not count has none. A preserved row that
			// a fresh session row later claims gets the live counts grafted on
			// (#4888 review); carrying the stale tallies instead would keep
			// dead sessions on screen for as long as the registry stays
			// degraded.
			r.SessionCount = 0
			r.InPlaceCount = 0
			registry = append(registry, r)
		}
	}
	return registry
}

// noteProjectDeleted records that this session's own completed mutation
// established repoID's deletion — the whole row is gone, live session counts
// included (a delete archives the project's sessions with it).
func (m *home) noteProjectDeleted(repoID string) {
	if repoID == "" {
		return
	}
	if m.goneProjectRepoIDs == nil {
		m.goneProjectRepoIDs = map[string]bool{}
	}
	m.goneProjectRepoIDs[repoID] = true
}

// noteProjectRebound records that this session's own completed mutation moved
// the registry record id — the row sheds its stale registry identity (the old
// root and checkout id), while a row still carrying live session counts keeps
// them as a session-only row: the move touched the registration, not the repo.
func (m *home) noteProjectRebound(registryID string) {
	if registryID == "" {
		return
	}
	if m.goneProjectRegistryIDs == nil {
		m.goneProjectRegistryIDs = map[string]bool{}
	}
	m.goneProjectRegistryIDs[registryID] = true
}

// clearGoneProjectRows drops the recorded absences a clean registry read
// renders true on its own — the union then states the truth itself, and a
// lingering tombstone would filter a row a re-registration revived.
func (m *home) clearGoneProjectRows() {
	m.goneProjectRepoIDs = nil
	m.goneProjectRegistryIDs = nil
}

// dropSessionGoneRows removes from a preserved row set the rows THIS session
// itself established as gone (#7522 review). A degraded-registry merge
// preserves a last-known row because the failed read cannot prove its absence
// — but the session proved THESE absences by performing the mutations, so the
// merge must not resurrect them while the registry stays unreadable. A
// deleted project's row drops outright (its sessions went with it); a rebound
// record's row sheds only the stale registry identity, keeping live session
// counts as a session-only row — and a sessionless rebound row drops, the way
// the merge already treats a registry row whose record proves gone.
func (m *home) dropSessionGoneRows(old []overlay.Project) []overlay.Project {
	if len(m.goneProjectRepoIDs) == 0 && len(m.goneProjectRegistryIDs) == 0 {
		return old
	}
	kept := make([]overlay.Project, 0, len(old))
	for _, row := range old {
		if m.goneProjectRepoIDs[row.RepoID] {
			continue
		}
		if row.RegistryID != "" && m.goneProjectRegistryIDs[row.RegistryID] {
			if row.SessionCount == 0 {
				continue
			}
			row.RegistryID = ""
			row.RegistryRoot = ""
			row.RegistryCheckoutID = ""
			row.MissingPath = false
		}
		kept = append(kept, row)
	}
	return kept
}
