// The view layer of the web client (#1592 Phase 5). It renders two views into
// #app: the paste-token login (design §1.2) and the authed app — a left rail of
// live sessions (PR3) beside a main pane that now hosts the live attach terminal
// (PR4). The rail mirrors the TUI sidebar (ui/sidebar_render.go): a status dot per
// row from the daemon projection, the title with the TUI's [lost]/[deleting]/
// [limit]/[remote] prefixes, and the branch as the secondary line.
//
// Rendering is direct DOM via a tiny `h()` helper (no framework, design §3.1) and
// is strictly CSP-safe: no inline scripts, no inline event handlers, no innerHTML
// with markup — everything is createElement + addEventListener.
//
// The app phase is rendered by an AppShell that patches the DOM IN PLACE rather
// than rebuilding the tree on every store update. This matters now that the main
// pane holds a stateful, focused xterm: a full rebuild on each session.* event
// would detach the terminal's <textarea> and steal keyboard focus mid-type. So the
// shell keeps ONE persistent terminal-host node and only re-touches the parts that
// actually changed — the rail list, the live pip, the pane header — leaving the
// terminal host (and its focus/scrollback) untouched except when the SELECTED
// session changes (a deliberate user act that rebuilds the terminal anyway).

import type { EventStreamStatus } from "./events.js";
import { isArchived, rowStatus, rowTitle } from "./status.js";
import type { TerminalStatus } from "./terminal.js";
import type { SessionData } from "./types.js";

/** The whole client state: which view to show, the login details, and — once
 *  authed — the live session projection plus the current selection. */
export interface AppState {
  phase: "login" | "app";
  /** true while a token probe is in flight (disables the login form). */
  connecting: boolean;
  /** an actionable message shown under the login form after a failed probe. */
  loginError: string | null;
  /** the live session projection (Snapshot + /v1/events), the rail's data. */
  sessions: SessionData[];
  /** the selected row's STABLE id (session.id), or null when nothing is selected.
   *  Selection keys off the id, not the title: the attach terminal dials
   *  /v1/sessions/{id}/stream and titles can collide across repos (design §4). */
  selectedId: string | null;
  /** the /v1/events connection state, shown as a small header indicator. */
  live: EventStreamStatus;
  /** the attach terminal's connection state, shown in the pane header. */
  termStatus: TerminalStatus;
}

/** Callbacks the shell invokes; index.ts owns the real behavior. */
export interface Actions {
  connect(token: string): void;
  disconnect(): void;
  /** Selects a session by its stable id (null-safe: rows without an id are inert). */
  select(id: string): void;
}

/** Minimal hyperscript: create an element, apply props, append children. Keeps the
 *  views declarative without a framework and without innerHTML. */
function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: Partial<HTMLElementTagNameMap[K]> & { class?: string } = {},
  ...children: (Node | string)[]
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key === "class") {
      el.className = value as string;
    } else {
      // Assign DOM properties (className, textContent, type, value, disabled…).
      (el as unknown as Record<string, unknown>)[key] = value;
    }
  }
  for (const child of children) {
    el.append(child);
  }
  return el;
}

/**
 * The rail's session order, mirroring the TUI sidebar: live sessions first, the
 * archived group last (ui/tree groups Archived under its own section), each group
 * ordered by creation time so the list is stable across re-renders and events.
 * Exported so keyboard navigation (index.ts) walks the SAME order the DOM shows.
 */
export function orderedSessions(sessions: SessionData[]): SessionData[] {
  return [...sessions].sort((a, b) => {
    const aa = isArchived(a) ? 1 : 0;
    const bb = isArchived(b) ? 1 : 0;
    if (aa !== bb) {
      return aa - bb;
    }
    const at = a.created_at ?? "";
    const bt = b.created_at ?? "";
    if (at !== bt) {
      return at < bt ? -1 : 1;
    }
    return a.title < b.title ? -1 : a.title > b.title ? 1 : 0;
  });
}

/** Renders the paste-token login view, replacing the root's contents. */
export function renderLogin(root: HTMLElement, state: AppState, actions: Actions): void {
  root.replaceChildren(loginView(state, actions));
}

function loginView(state: AppState, actions: Actions): HTMLElement {
  const input = h("input", {
    type: "password",
    id: "af-token",
    placeholder: "Paste your daemon token",
    autocomplete: "off",
    disabled: state.connecting,
  });
  input.setAttribute("aria-label", "Daemon bearer token");

  const button = h(
    "button",
    { type: "submit", class: "af-primary", disabled: state.connecting },
    state.connecting ? "Connecting…" : "Connect",
  );

  const form = h(
    "form",
    { class: "af-login-form" },
    h("label", { class: "af-field-label", htmlFor: "af-token" }, "Daemon token"),
    input,
    button,
  );
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const token = input.value.trim();
    if (token !== "") {
      actions.connect(token);
    }
  });

  const children: (Node | string)[] = [
    h("h1", { class: "af-title" }, "Agent Factory"),
    h(
      "p",
      { class: "af-subtitle" },
      "Paste the daemon bearer token to connect. Get it from ",
      h("code", {}, "af token show"),
      " on the host.",
    ),
    form,
  ];
  if (state.loginError) {
    children.push(h("p", { class: "af-error", role: "alert" }, state.loginError));
  }

  return h("main", { class: "af-login" }, h("div", { class: "af-card" }, ...children));
}

/** The label a terminal status reads as in the pane header. */
function termStatusLabel(s: TerminalStatus): string {
  switch (s) {
    case "open":
      return "Live";
    case "connecting":
      return "Connecting…";
    case "reconnecting":
      return "Reconnecting…";
    case "exited":
      return "Agent exited";
  }
}

/**
 * The authed-app DOM, built once and patched in place. index.ts constructs one of
 * these when the login succeeds, appends `.el` to #app, and calls update(state) on
 * every store change. The terminal host node it is handed lives permanently in the
 * pane and is only reparented when the selected session changes — so live events
 * that rebuild the rail never disturb the focused terminal.
 */
export class AppShell {
  readonly el: HTMLElement;
  private readonly pip: HTMLElement;
  private readonly pipLabel: HTMLElement;
  private readonly railCount: HTMLElement;
  private readonly railList: HTMLElement;
  private readonly main: HTMLElement;

  // Header text nodes for the selected pane, (re)created per selection.
  private headTitle: HTMLElement | null = null;
  private headMeta: HTMLElement | null = null;

  // Last-applied state, for cheap change detection between updates.
  private lastSessions: SessionData[] | null = null;
  private lastSelectedId: string | null = null;
  private lastLive: EventStreamStatus | null = null;

  constructor(
    private readonly actions: Actions,
    private readonly termHost: HTMLElement,
  ) {
    this.pip = h("span", { class: "af-live-pip" });
    this.pip.setAttribute("aria-hidden", "true");
    this.pipLabel = h("span", { class: "af-live-label" });
    const live = h("span", { class: "af-live" }, this.pip, this.pipLabel);
    live.setAttribute("role", "status");

    const disconnect = h("button", { type: "button", class: "af-ghost" }, "Disconnect");
    disconnect.addEventListener("click", () => this.actions.disconnect());

    const header = h(
      "header",
      { class: "af-appbar" },
      h("span", { class: "af-brand" }, "Agent Factory"),
      live,
      disconnect,
    );

    this.railCount = h("span", { class: "af-rail-count" }, "0");
    const railHead = h(
      "div",
      { class: "af-rail-head" },
      h("span", { class: "af-rail-title" }, "Sessions"),
      this.railCount,
    );
    this.railList = h("ul", { class: "af-rail-list" });
    this.railList.setAttribute("role", "listbox");
    this.railList.setAttribute("aria-label", "Sessions");
    const rail = h("nav", { class: "af-rail" }, railHead, this.railList);

    this.main = h("section", { class: "af-main" });
    const body = h("div", { class: "af-body" }, rail, this.main);
    this.el = h("main", { class: "af-app" }, header, body);
  }

  /** Applies the latest state, touching only what changed. */
  update(state: AppState): void {
    if (this.lastLive !== state.live) {
      this.lastLive = state.live;
      this.pip.className = `af-live-pip af-live-${state.live}`;
      this.pipLabel.textContent =
        state.live === "open" ? "Live" : state.live === "connecting" ? "Connecting…" : "Reconnecting…";
    }

    const sessionsChanged = this.lastSessions !== state.sessions;
    const selectionChanged = this.lastSelectedId !== state.selectedId;
    this.lastSessions = state.sessions;
    this.lastSelectedId = state.selectedId;

    // Rebuild the rail when the list OR the highlighted row changed. Neither touches
    // the terminal host (it lives in the main pane), so events never blur it.
    if (sessionsChanged || selectionChanged) {
      this.renderRail(state);
    }

    // The main pane's STRUCTURE only changes when the selected session changes;
    // otherwise we just patch its header text (status/title/branch), leaving the
    // terminal host — and its focus and scrollback — in place.
    if (selectionChanged) {
      this.renderMain(state);
    } else {
      this.patchMainHead(state);
    }
  }

  private renderRail(state: AppState): void {
    this.railCount.textContent = String(state.sessions.length);
    const list = this.railList;
    if (state.sessions.length === 0) {
      list.replaceChildren(
        h(
          "li",
          { class: "af-rail-empty" },
          "No sessions yet. Create one in the TUI or with ",
          h("code", {}, "af sessions create"),
          ".",
        ),
      );
      return;
    }
    const rows = orderedSessions(state.sessions).map((s) =>
      sessionRow(s, s.id === state.selectedId, this.actions),
    );
    list.replaceChildren(...rows);
  }

  private renderMain(state: AppState): void {
    const selected = selectedSession(state);
    if (!selected) {
      this.headTitle = null;
      this.headMeta = null;
      // Detaches the terminal host if it was mounted; index.ts disposes the terminal.
      this.main.className = "af-main af-main-empty";
      this.main.replaceChildren(
        h("p", { class: "af-empty-title" }, "Select a session"),
        h("p", { class: "af-empty-hint" }, "Pick a session in the rail to attach its terminal."),
      );
      return;
    }
    this.headTitle = h("span", { class: "af-term-title" }, selected.title);
    this.headMeta = h("span", { class: "af-term-meta" });
    const head = h("div", { class: "af-term-head" }, this.headTitle, this.headMeta);
    this.main.className = "af-main af-main-term";
    // The persistent terminal host is (re)mounted here; renderMain runs only on a
    // selection change, so this reparent is rare and never happens mid-type.
    this.main.replaceChildren(head, this.termHost);
    this.patchMainHead(state);
  }

  private patchMainHead(state: AppState): void {
    const selected = selectedSession(state);
    if (!selected || !this.headTitle || !this.headMeta) {
      return;
    }
    this.headTitle.textContent = selected.title;
    const parts = [termStatusLabel(state.termStatus)];
    if (selected.branch) {
      parts.push(selected.branch);
    }
    this.headMeta.textContent = parts.join(" · ");
    this.headMeta.className = `af-term-meta af-term-${state.termStatus}`;
  }
}

/** The currently selected session row, or null. */
function selectedSession(state: AppState): SessionData | null {
  return state.selectedId ? (state.sessions.find((s) => s.id === state.selectedId) ?? null) : null;
}

/** One session row: a status dot, the (prefixed) title, and the branch line —
 *  the same three signals the TUI row carries (ui/tree/render.go). Clicking selects
 *  by the stable id; a row lacking an id (never expected from a live Snapshot) is
 *  rendered but inert. */
function sessionRow(s: SessionData, selected: boolean, actions: Actions): HTMLElement {
  const status = rowStatus(s);
  const dot = h(
    "span",
    { class: `af-dot af-dot-${status.kind}${status.spinning ? " af-dot-spin" : ""}` },
    status.glyph,
  );
  dot.setAttribute("aria-hidden", "true");

  const title = h("div", { class: "af-row-title" }, rowTitle(s));
  const branch = h(
    "div",
    { class: "af-row-branch" },
    h("span", { class: "af-branch-icon" }, "⎇"),
    " ",
    s.branch || "—",
  );

  const cls = `af-row${selected ? " af-row-selected" : ""}${isArchived(s) ? " af-row-archived" : ""}`;
  const row = h("li", { class: cls }, dot, h("div", { class: "af-row-main" }, title, branch));
  row.setAttribute("role", "option");
  row.setAttribute("aria-selected", selected ? "true" : "false");
  row.setAttribute("title", `${s.title} — ${status.label}`);
  if (s.id) {
    const id = s.id;
    row.addEventListener("click", () => actions.select(id));
  }
  return row;
}
