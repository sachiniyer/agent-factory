// The view layer of the web client (#1592 Phase 5). It renders two views into
// #app: the paste-token login (design §1.2) and the authed app — a left rail of
// live sessions (PR3) beside a main content area (the terminal lands in PR4). The
// rail mirrors the TUI sidebar (ui/sidebar_render.go): a status dot per row from
// the daemon projection, the title with the TUI's [lost]/[deleting]/[limit]/
// [remote] prefixes, and the branch as the secondary line. It is a pure projection
// of store state fed by Snapshot + /v1/events — no client-side source of truth.
//
// Rendering is direct DOM via a tiny `h()` helper (no framework, design §3.1) and
// is strictly CSP-safe: no inline scripts, no inline event handlers, no innerHTML
// with markup — everything is createElement + addEventListener, so the served
// `Content-Security-Policy: default-src 'self'` holds.

import type { EventStreamStatus } from "./events.js";
import { isArchived, rowStatus, rowTitle } from "./status.js";
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
  /** the selected row's identity (session title; titles are unique in af), or
   *  null when nothing is selected. */
  selectedTitle: string | null;
  /** the /v1/events connection state, shown as a small header indicator. */
  live: EventStreamStatus;
}

/** Callbacks the shell invokes; index.ts owns the real behavior. */
export interface Actions {
  connect(token: string): void;
  disconnect(): void;
  select(title: string): void;
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

/** Renders the current state into the given root, replacing its contents. */
export function render(root: HTMLElement, state: AppState, actions: Actions): void {
  root.replaceChildren(state.phase === "login" ? loginView(state, actions) : appView(state, actions));
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

function appView(state: AppState, actions: Actions): HTMLElement {
  const disconnect = h("button", { type: "button", class: "af-ghost" }, "Disconnect");
  disconnect.addEventListener("click", () => actions.disconnect());

  const header = h(
    "header",
    { class: "af-appbar" },
    h("span", { class: "af-brand" }, "Agent Factory"),
    liveIndicator(state.live),
    disconnect,
  );

  const body = h("div", { class: "af-body" }, sidebar(state, actions), mainPane(state));
  return h("main", { class: "af-app" }, header, body);
}

/** The events-stream health pip in the header: green when the push stream is open,
 *  amber while (re)connecting — so "live updates are flowing" is legible at a glance. */
function liveIndicator(live: EventStreamStatus): HTMLElement {
  const label = live === "open" ? "Live" : live === "connecting" ? "Connecting…" : "Reconnecting…";
  const pip = h("span", { class: `af-live-pip af-live-${live}` });
  pip.setAttribute("aria-hidden", "true");
  const wrap = h("span", { class: "af-live" }, pip, h("span", { class: "af-live-label" }, label));
  wrap.setAttribute("role", "status");
  return wrap;
}

/** The left rail: a header with the session count and a row per session. */
function sidebar(state: AppState, actions: Actions): HTMLElement {
  const count = state.sessions.length;
  const head = h(
    "div",
    { class: "af-rail-head" },
    h("span", { class: "af-rail-title" }, "Sessions"),
    h("span", { class: "af-rail-count" }, String(count)),
  );

  const list = h("ul", { class: "af-rail-list" });
  list.setAttribute("role", "listbox");
  list.setAttribute("aria-label", "Sessions");
  if (count === 0) {
    list.append(
      h(
        "li",
        { class: "af-rail-empty" },
        "No sessions yet. Create one in the TUI or with ",
        h("code", {}, "af sessions create"),
        ".",
      ),
    );
  } else {
    for (const s of orderedSessions(state.sessions)) {
      list.append(sessionRow(s, s.title === state.selectedTitle, actions));
    }
  }

  return h("nav", { class: "af-rail" }, head, list);
}

/** One session row: a status dot, the (prefixed) title, and the branch line —
 *  the same three signals the TUI row carries (ui/tree/render.go). */
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
  row.addEventListener("click", () => actions.select(s.title));
  return row;
}

/** The main content area — an empty state until the terminal lands in PR4. It
 *  names the selected session so selection is visibly wired end-to-end. */
function mainPane(state: AppState): HTMLElement {
  const selected = state.sessions.find((s) => s.title === state.selectedTitle) ?? null;
  if (!selected) {
    return h(
      "section",
      { class: "af-main af-main-empty" },
      h("p", { class: "af-empty-title" }, "Select a session"),
      h("p", { class: "af-empty-hint" }, "The terminal arrives in the next update."),
    );
  }
  const status = rowStatus(selected);
  return h(
    "section",
    { class: "af-main af-main-empty" },
    h("p", { class: "af-empty-title" }, selected.title),
    h(
      "p",
      { class: "af-empty-hint" },
      `${status.label}${selected.branch ? ` · ${selected.branch}` : ""}`,
    ),
    h("p", { class: "af-empty-hint" }, "The terminal for this session arrives in the next update."),
  );
}
