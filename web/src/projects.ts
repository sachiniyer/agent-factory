// The PROJECTS view of the web client (#1592 Phase 5 PR8): the browser analogue of
// the TUI's projects pane (ui/projects.go). It groups the live session projection by
// repo root — one section per project af has seen — so the browser reads the repo
// grouping the same way the TUI does. Projects key by REPO PATH (the stable project
// id, mirroring app/switch_project.go buildProjectListFrom); the derivation is the
// same distinct-repo-roots walk the new-session picker uses (ui.ts deriveProjects),
// never an invented client id.
//
// It is a read-and-jump surface, not a mutation one: clicking a session under its
// project opens it — which switches back to the sessions view and attaches its
// terminal (index.ts onOpen) — so the projects pane doubles as a per-project session
// switcher. It is patched in place (build once, re-render on a sessions/selection
// change) like the rest of the shell, and scrolls rather than wrapping so a long
// project list never pushes content off-screen (the #1620 vim-scrollable behavior).
//
// CSP-safe like the rest of the client: createElement + addEventListener only (the
// shared h() helper), no innerHTML with markup.

import { h, projectLabel } from "./modals.js";
import { compareSessionsForRail, isArchived, rowStatus, rowTitle } from "./status.js";
import type { SessionData } from "./types.js";

/** One project section: its repo root (the stable id), a friendly label, and the
 *  sessions grouped under it in rail order. */
export interface ProjectGroup {
  /** The repo root — the stable project id every project keys by. */
  root: string;
  /** The friendly label (repo basename + parent), shared with the modal picker. */
  label: string;
  /** The project's sessions in rail order: live rows oldest-first, then the archived
   *  group newest-first (compareSessionsForRail). */
  sessions: SessionData[];
}

/** Orders sessions within a project the same way the rail does — the shared
 *  compareSessionsForRail (status.ts): live rows oldest-first before the archived
 *  group newest-first (#1605), so the projects pane and the sessions rail can never
 *  diverge on order (#1674 PR3 review). */
function orderWithinProject(sessions: SessionData[]): SessionData[] {
  return [...sessions].sort(compareSessionsForRail);
}

/**
 * Groups the live sessions by their repo root (project), sorted by path for a
 * stable menu — the browser analogue of buildProjectListFrom. A session with no
 * repo_path is skipped: it can't be attributed to a project (the same rows the
 * new-session picker omits). Exported so index.ts can flatten the SAME order for
 * anything that needs it, and unit tests can pin the grouping.
 */
export function groupSessionsByProject(sessions: SessionData[]): ProjectGroup[] {
  const byRoot = new Map<string, SessionData[]>();
  for (const s of sessions) {
    // Only LIVE sessions define an "active project" (#1735): an archived session
    // belongs to the sessions rail's Archived group, not the projects view. This
    // is what makes delete-project (archive every live session) drop the project
    // row, and restoring an archived session bring it back — the reversible
    // contract. It mirrors the TUI's live-only buildProjectListFrom.
    if (isArchived(s)) {
      continue;
    }
    const root = s.worktree?.repo_path;
    if (!root) {
      continue;
    }
    const arr = byRoot.get(root) ?? [];
    arr.push(s);
    byRoot.set(root, arr);
  }
  return [...byRoot.keys()]
    .sort()
    .map((root) => ({ root, label: projectLabel(root), sessions: orderWithinProject(byRoot.get(root) ?? []) }));
}

/**
 * The projects pane: build once (its `el` mounts into the app body beside the
 * sessions body), then update(sessions, selectedId) re-renders on a change. Kept as
 * a small stateful class like the terminal/tab bar so a live rail event patches only
 * this subtree, never the sibling terminal host.
 */
export class ProjectsPane {
  readonly el: HTMLElement;
  private lastSessions: SessionData[] | null = null;
  private lastSelectedId: string | null = null;

  /** onOpen selects + attaches a session by its stable id (index.ts switches to the
   *  sessions view and hands the terminal the keyboard), so a project's session row
   *  is a jump-to-session affordance. onDeleteProject deletes a project by its repo
   *  root (#1735) — index.ts confirms first, then calls the DeleteProject RPC. */
  constructor(
    private readonly onOpen: (id: string) => void,
    private readonly onDeleteProject: (root: string, label: string, sessionCount: number) => void,
  ) {
    this.el = h("section", { class: "af-projects" });
    this.el.setAttribute("aria-label", "Projects");
  }

  /** Re-renders when the session list or the selection changed. */
  update(sessions: SessionData[], selectedId: string | null): void {
    if (this.lastSessions === sessions && this.lastSelectedId === selectedId) {
      return;
    }
    this.lastSessions = sessions;
    this.lastSelectedId = selectedId;
    this.render(sessions, selectedId);
  }

  private render(sessions: SessionData[], selectedId: string | null): void {
    const groups = groupSessionsByProject(sessions);
    const head = h(
      "div",
      { class: "af-projects-head" },
      h("span", { class: "af-projects-title" }, "Projects"),
      h("span", { class: "af-view-count" }, String(groups.length)),
    );
    if (groups.length === 0) {
      this.el.replaceChildren(
        head,
        h(
          "p",
          { class: "af-projects-empty" },
          "No projects yet. Create a session in a repo (the TUI or ",
          h("code", {}, "af sessions create"),
          ") and it appears here.",
        ),
      );
      return;
    }
    const sections = groups.map((g) => this.projectSection(g, selectedId));
    this.el.replaceChildren(head, h("div", { class: "af-projects-list" }, ...sections));
  }

  private projectSection(group: ProjectGroup, selectedId: string | null): HTMLElement {
    // The reversible delete-project control (#1735): a ghost button on the header,
    // mirroring the terminal-head Archive button. index.ts pops the confirm modal.
    const deleteBtn = h("button", { type: "button", class: "af-ghost af-project-delete" }, "Delete");
    deleteBtn.setAttribute("title", `Delete project ${group.label} (archives its sessions, restorable)`);
    deleteBtn.setAttribute("aria-label", `Delete project ${group.label}`);
    deleteBtn.addEventListener("click", () => this.onDeleteProject(group.root, group.label, group.sessions.length));
    const header = h(
      "div",
      { class: "af-project-head" },
      h("span", { class: "af-project-name" }, group.label),
      h("span", { class: "af-project-count" }, `${group.sessions.length}`),
      deleteBtn,
    );
    header.append(h("div", { class: "af-project-path" }, group.root));
    const rows = group.sessions.map((s) => this.sessionRow(s, s.id === selectedId));
    return h("section", { class: "af-project" }, header, h("ul", { class: "af-project-sessions" }, ...rows));
  }

  /** One session row under a project: the same status dot + prefixed title the rail
   *  row carries (status.ts), keyed by the stable id. Clicking opens it (→ sessions
   *  view). A row with no id (never from a live Snapshot) renders but is inert. */
  private sessionRow(s: SessionData, selected: boolean): HTMLElement {
    const status = rowStatus(s);
    const dot = h(
      "span",
      { class: `af-dot af-dot-${status.kind}${status.spinning ? " af-dot-spin" : ""}` },
      status.glyph,
    );
    dot.setAttribute("aria-hidden", "true");
    const title = h("div", { class: "af-row-title" }, rowTitle(s));
    const cls = `af-row af-project-row${selected ? " af-row-selected" : ""}${isArchived(s) ? " af-row-archived" : ""}`;
    const row = h("li", { class: cls }, dot, h("div", { class: "af-row-main" }, title));
    row.setAttribute("role", "button");
    row.setAttribute("title", `${s.title} — ${status.label}`);
    if (s.id) {
      const id = s.id;
      row.addEventListener("click", () => this.onOpen(id));
    }
    return row;
  }
}
