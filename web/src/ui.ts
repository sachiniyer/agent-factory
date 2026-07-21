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
import type { KeyboardFocus, View } from "./nav.js";
import { VIEWS } from "./nav.js";
import { resolveDragTab, TAB_DND_MIME } from "./layout.js";
import {
  FILTER_KINDS,
  filterLabel,
  filterSessions,
  hiddenCount,
  isDefaultFilter,
  kindCounts,
  type StatusFilter,
} from "./filter.js";
import { projectMeta, projectName, type ProjectSummary, projectSummaries, scopeToProject } from "./project.js";
import {
  compareSessionsForRail,
  isArchived,
  isCreating,
  isLimitReached,
  type RowKind,
  rowStatus,
  rowTitle,
} from "./status.js";
import { ConfigPane, type ConfigStatus } from "./config.js";
import { isRenameableTab, tabDisplayLabel, tabGlyph, tabLabel } from "./tablabel.js";
import { insertionIndexAt, reorderTargetIndex } from "./tabreorder.js";
import { TasksPane } from "./tasks.js";
import { type ThemeChoice, THEME_CHOICES } from "./theme.js";
import type { TerminalStatus } from "./terminal.js";
import { type ConfigEntry, type LifecycleAction, type SessionData, type TaskData, TabKind } from "./types.js";

/** The tab kinds the web UI can create with no further input from the user. A
 *  web tab is deliberately absent: its target comes from whatever an agent is
 *  running, so it stays CLI/API-created (see docs/web.md). */
export type NewTabKind = "shell" | "vscode";

/** A row carrying the Go domain's positive lifecycle capability and a stable
 *  destructive-action target. Lifecycle callbacks accept only this narrowed type,
 *  so a visible but inert row cannot reach them through the normal code path. */
export type ActionableSession = SessionData & { id: string; lifecycle_action: LifecycleAction };

/** Fail-closed wire narrowing only — the policy itself lives in Go's
 *  session.LifecycleAction (#2234). Exact values reject a malformed or stale
 *  projection instead of manufacturing an action in the browser. */
export function isActionableSession(s: SessionData): s is ActionableSession {
  return (
    typeof s.id === "string" &&
    s.id !== "" &&
    (s.lifecycle_action === "archive" || s.lifecycle_action === "restore")
  );
}

/** The whole client state: which view to show, the login details, and — once
 *  authed — the live session projection plus the current selection. */
export interface AppState {
  phase: "login" | "app";
  /** the top-level view: the live sessions rail+terminal, or the tasks (scheduled
   *  automations) pane — both SCOPED to the selected project (redesign PR2). The
   *  appbar view tabs and the [ / ] keys switch it; it selects which body shows. */
  view: View;
  /** the selected project's repo root (redesign PR2), or null when there are no
   *  projects at all. The whole UI is scoped to it: the rail lists only this
   *  project's sessions and the tasks view only its tasks. The top-right switcher
   *  changes it; it is derived client-side by grouping sessions by repo and persisted
   *  to localStorage so it survives a reload. */
  selectedProject: string | null;
  /** whether THIS client must present a bearer token, from the /v1/auth-info probe
   *  (#1696). false ⇒ the daemon exempts this peer (loopback, or require_token=false),
   *  so the login view offers a one-click tokenless connect instead of the paste form.
   *  Defaults true so the paste form is shown until the probe says otherwise. */
  authRequired: boolean;
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
  /** which pane owns the keyboard (#1693): "rail" is the default nav mode (j/k move
   *  the selection); "terminal" means keys go to the agent until Escape. Drives the
   *  visible focus indicator so the active mode is legible, mirroring the TUI. */
  focus: KeyboardFocus;
  /** the selected session's active tab index (#1592 Phase 5 PR7): 0 is the agent
   *  tab, 1-8 the user-created shell/process tabs. The attach terminal streams
   *  /stream?tab=<activeTab>, and the tab bar highlights it. Reset to 0 whenever
   *  the selection changes. With split panes (feat: drag-and-drop split tabs) this
   *  tracks the FOCUSED pane's tab, so the tab bar highlights the tab the keyboard
   *  is on. */
  activeTab: number;
  /** the tabs currently shown across ALL panes of the selected session (split.ts's
   *  layout mirror), so the tab bar can flag which tabs are already open in a pane.
   *  Defaults to [activeTab] — one pane — for the unsplit case. */
  shownTabs: number[];
  /** an actionable message shown as a transient toast when a tab op (create/close)
   *  or a task op (toggle/trigger/remove) fails, or null when there is none. These
   *  ops have no modal to surface an error in (unlike create/kill/archive), so the
   *  failure is shown here instead of being silently swallowed (#1592 Phase 5 PR7/PR8). */
  tabError: string | null;
  /** the live task projection (ListTasks + task.* events), the tasks view's data. */
  tasks: TaskData[];
  /** the config manifest zipped with the user's live values (GetConfig), the config
   *  view's data. There is no local key list: this IS the description of config, so
   *  a key added to config_types.go arrives here with no change to the bundle. */
  config: ConfigEntry[];
  /** the config.toml the values were read from, named in the config view so an
   *  AF_HOME user knows which file they are editing. */
  configPath: string;
  /** the outcome of the last config write — the daemon's echo and restart notice,
   *  or the validator's message when it refused — or null when there is none. */
  configStatus: ConfigStatus | null;
  /** the persisted theme preference (redesign PR1): Auto follows the OS, Light/Dark
   *  force a mode. The appbar toggle sets it; theme.ts stamps data-theme on <html>
   *  and re-themes the live terminals. */
  themeChoice: ThemeChoice;
  /** which session states the rail shows (feat: hide archived by default). A pure
   *  client-side DISPLAY filter over the projection — archived sessions still arrive
   *  in every Snapshot, the rail just doesn't draw them unless asked. Defaults to
   *  every state but archived, is set by the rail's filter menu, and is persisted to
   *  localStorage (filter.ts) so the choice sticks per browser. */
  statusFilter: StatusFilter;
}

/** Callbacks the shell invokes; index.ts owns the real behavior. */
export interface Actions {
  connect(token: string): void;
  disconnect(): void;
  /** Opens a session by its stable id (null-safe: rows without an id are inert):
   *  selects the row AND hands the keyboard to its terminal, so a mouse click
   *  attaches exactly like Enter on the selected row (#1693). */
  open(id: string): void;
  /** Opens the new-session modal (#1592 Phase 5 PR5). */
  newSession(): void;
  /** Opens the kill-confirm modal for this rail row. */
  kill(session: ActionableSession): void;
  /** Opens the archive-confirm modal for this rail row. */
  archive(session: ActionableSession): void;
  /** Opens the restore-confirm modal for this rail row (an archived / Lost / Dead
   *  session): the reverse of archive (#1932). */
  restore(session: ActionableSession): void;
  /** Resumes the current selection from its usage-limit wall (#1934) — the web's
   *  analogue of the TUI's `c`. Fires immediately with NO confirm, matching the
   *  TUI: it is not destructive (it re-delivers the prompt the session was already
   *  going to run) and it is the obvious next step for a session that is stuck. */
  retryLimit(): void;
  /** Switches the selected session's active tab WITHOUT attaching — the keyboard
   *  stays in rail nav mode (the 1-9 keys, mirroring the TUI). */
  switchTab(index: number): void;
  /** Switches to a tab AND attaches its terminal (a tab-bar click, mirroring how a
   *  session-row click attaches). */
  openTab(index: number): void;
  /** Creates a new tab on the selected session (the `t` key / + menu): a $SHELL
   *  terminal, or a VS Code editor on the session's worktree. */
  newTab(kind: NewTabKind): void;
  /** Closes the tab at `index` of the selected session (the `w` key / × button);
   *  the agent tab (index 0) is unclosable. */
  closeTab(index: number): void;
  /** Sets one global config key. index.ts POSTs SetConfigValue (the same validated,
   *  locked, atomic writer `af config set` uses), then re-reads the manifest so the
   *  form shows what the file actually holds. Validation is deliberately NOT done
   *  in the browser: a second copy of the rules is how a UI accepts a value the
   *  loader later rejects at startup. */
  setConfigValue(key: string, value: string): void;
  /** Renames the tab with the stable IDENTITY `id` (tabIdentity) to `name` (#1813):
   *  the commit of the bar's inline edit. Only offered for a RENAMEABLE tab (web /
   *  process — see isRenameableTab); index.ts resolves the identity to the tab's
   *  CURRENT ordinal and name and calls the daemon, which returns the RESOLVED name
   *  (it may dup-suffix, `dup` → `dup-2`) and publishes session.updated for every
   *  client to repaint from.
   *
   *  An identity, NOT the ordinal the edit began at: an edit and its commit span two
   *  rosters (the tab list can be reordered or shrunk by another window WHILE the
   *  input is open — and that repaint is itself what blurs the input and commits it),
   *  so an ordinal would name whichever tab had since moved into the slot. Same
   *  principle, and the same reason, as closeSessionTab's tabToKeepOnClose.
   *
   *  The identity is the one CAPTURED WHEN THE EDIT OPENED (beginTabRename), so `id`
   *  may name a tab that no longer exists — another client can close it while the input
   *  is open. That is a MISS: index.ts reports it and renames nothing, rather than
   *  falling through to whatever now occupies the slot.
   *
   *  `sessionId` is the session the edit OPENED on, also captured at edit start. A
   *  render that reparents the bar (the user switching session/project) blurs the open
   *  input and so fires this commit against the NOW-selected session — where the edited
   *  tab legitimately does not exist. That is the user's own navigation, not a vanished
   *  tab, so index.ts abandons it silently rather than reporting a tab "gone" that is
   *  still there in the session they left. The miss is only reported when the session
   *  is unchanged. */
  renameTab(id: string, name: string, sessionId: string): void;
  /** Moves the tab at `from` to the 0-based `to` (#1813): the commit of a drag
   *  within the tab bar. The agent tab is pinned at 0 — neither index may be it,
   *  which the bar enforces before calling and the daemon refuses regardless. */
  reorderTab(from: number, to: number): void;
  /** Switches the top-level view: the appbar view tabs and the [ / ] keys route
   *  here; index.ts flips the store and hands the keyboard back to the rail (blurring
   *  the terminal) when leaving the sessions view. */
  switchView(view: View): void;
  /** Switches the active project (redesign PR2): the top-right switcher menu routes
   *  here; index.ts scopes the rail + views to `root`, persists it, and drops a
   *  selection that no longer belongs to the new project. */
  switchProject(root: string): void;
  /** Shows/hides one session state in the rail (feat: hide archived by default): the
   *  filter menu's checkboxes and the empty state's inline "Show archived" route
   *  here; index.ts flips the store and persists the whole filter. */
  setStatusFilter(kind: RowKind, on: boolean): void;
  /** Restores the default filter — every state but archived. */
  resetStatusFilter(): void;
  /** Opens the add-task modal (#1592 Phase 5 PR8). */
  addTask(): void;
  /** Opens the edit-task modal seeded from the task, submitting via UpdateTask (#1935). */
  editTask(task: TaskData): void;
  /** Enables/disables a task via UpdateTask. */
  toggleTask(task: TaskData): void;
  /** Fires a task now via TriggerTask (enabled cron tasks). */
  triggerTask(task: TaskData): void;
  /** Removes a task via RemoveTask. */
  removeTask(task: TaskData): void;
  /** Opens the reversible delete-project confirm for a project row (#1735); on
   *  confirm index.ts calls DeleteProject, which archives its live sessions. */
  deleteProject(root: string, label: string, sessionCount: number): void;
  /** Sets the theme preference (redesign PR1): persists it, stamps data-theme on
   *  <html>, and re-themes the live terminals. */
  setTheme(choice: ThemeChoice): void;
}

/** The soft cap on tabs per session (session/tab.go maxTabs): the agent tab plus
 *  up to eight additional tabs, matching the 1-9 number-key range. At the cap the
 *  create control becomes an explanatory note, so the web never fires a
 *  guaranteed-to-fail CreateTab. */
const MAX_TABS = 9;

/** The backend types whose workspace lives off-box (session/archive_sandbox.go
 *  backendKindForType: docker, ssh, and "remote" — the hook runtime). None of
 *  them can service tab management: every Add*Tab path needs a daemon-side git
 *  worktree they do not have, so the daemon's Capabilities().TabManagement is
 *  false for all three (#1874).
 *
 *  This list is the ONE place the web names backend types. It exists only because
 *  the session envelope carries `backend_type` but not the daemon's capability
 *  bits — the honest fix is to serialize the capability so no client re-derives
 *  it (see #1874). Until then, a NEW off-box runtime must be added here, which is
 *  what ui.test.ts pins. */
const OFF_BOX_BACKENDS = new Set(["docker", "ssh", "remote"]);

/** Whether a session supports user tab management. Off-box sessions (docker/ssh/
 *  remote-hook) have a tab list their runtime fixes at launch — a single agent
 *  tab — so the web withdraws their × controls, replaces new-tab with a visible
 *  fixed-list explanation, and gates the `t`/`w` keys, mirroring the daemon's
 *  Capabilities().TabManagement.
 *
 *  A record with no backend_type is a pre-#1592 local session (the field is
 *  omitempty), so it defaults to local — treating it as off-box would strip tab
 *  management from every legacy row. */
export function supportsTabManagement(s: SessionData): boolean {
  return !OFF_BOX_BACKENDS.has(s.backend_type ?? "local");
}

/** Whether the web may offer tab management for a session RIGHT NOW: its backend
 *  must support it AND it must not be archived. An archived session is inert — the
 *  daemon refuses both CreateTab (since #1196) and CloseTab (#1809 follow-up) on
 *  one — so offering mutable + / × controls there can only produce a guaranteed-to-fail call, and
 *  the × specifically would try to strip the web-tab URL that archive preserved for
 *  the restore. Every site that offers or fires a tab mutation reads THIS, not
 *  supportsTabManagement, so the affordances and the daemon's answer can't drift. */
export function canManageTabs(s: SessionData): boolean {
  return supportsTabManagement(s) && !isArchived(s);
}

/** A visible explanation for every state in which the tab bar cannot offer its
 *  new-tab control. Returning null means creation is available. This stays
 *  separate from canManageTabs because "archived", "runtime-fixed", and "full"
 *  have different next steps and must not collapse into one missing affordance. */
export function tabCreationUnavailableReason(s: SessionData, tabCount = sessionTabs(s).length): string | null {
  const supported = supportsTabManagement(s);
  if (isArchived(s)) {
    if (!supported) {
      return `Archived · ${tabRuntimeLabel(s)} sessions have a fixed tab list`;
    }
    return "Restore this session to create tabs";
  }
  if (!supported) {
    return `${tabRuntimeLabel(s)} sessions have a fixed tab list`;
  }
  if (tabCount >= MAX_TABS) {
    return "Nine-tab limit reached";
  }
  return null;
}

function tabRuntimeLabel(s: SessionData): string {
  switch (s.backend_type) {
    case "docker":
      return "Docker";
    case "ssh":
      return "SSH";
    default:
      return "Remote";
  }
}

/** The selected session's tabs, always non-empty: a pre-#930 record with no tabs
 *  is shown as a single implicit agent tab so the bar (and index math) never sees
 *  an empty list. */
export function sessionTabs(s: SessionData): { id?: string; name: string; kind: number; url?: string }[] {
  if (s.tabs && s.tabs.length > 0) {
    return s.tabs;
  }
  return [{ name: "agent", kind: 0 }];
}

/** The STABLE IDENTITY for a tab (#1738): the daemon-minted, never-reused tab id
 *  (session.TabData.ID). It is what a stream (?tab_id=) and a DnD/pane binding
 *  address the tab by, so a reorder/close can't misroute — the drop resolves this
 *  id to the tab's CURRENT ordinal instead of trusting the drag-time position. Falls
 *  back to the legacy kind:name for a record written before #1738 (no id): still a
 *  per-tab string, just not collision-proof against a close+recreate of the same
 *  name — the exact residual #1738 closes once every record carries an id. */
export function tabIdentity(tab: { id?: string; name: string; kind: number }): string {
  return tab.id && tab.id !== "" ? tab.id : `${tab.kind}:${tab.name}`;
}

/** The REAL daemon tab id, or "" when the tab has none (#1779).
 *
 *  This is deliberately NOT tabIdentity. The two answer different questions and
 *  conflating them is a bug in both directions:
 *
 *  - tabIdentity answers "is this the same tab as before?" for LOCAL bookkeeping
 *    (reconcile, mid-drag change detection). It must always return something, so
 *    it synthesizes a `kind:name` fallback for an id-less tab.
 *  - tabRealId answers "what id may I hand to the DAEMON?" — and a synthesized
 *    `1:shell` is not an answer. The daemon 404s an unknown non-empty ?tab_id=,
 *    so passing a fallback breaks a legacy tab that would otherwise attach fine by
 *    ordinal. "" is the honest "no id" that makes callers fall back to ?tab=.
 *
 *  Anything crossing the wire, or claiming an identity is collision-proof, uses
 *  this; the fallback is only ever a local hint. */
export function tabRealId(tab: { id?: string }): string {
  return tab.id && tab.id !== "" ? tab.id : "";
}

/** The appbar label for a top-level view. */
function viewLabel(view: View): string {
  switch (view) {
    case "sessions":
      return "Sessions";
    case "tasks":
      return "Tasks";
    case "config":
      return "Config";
  }
}

/** The appbar label for a theme choice (redesign PR1). */
function themeLabel(choice: ThemeChoice): string {
  switch (choice) {
    case "auto":
      return "Auto";
    case "light":
      return "Light";
    case "dark":
      return "Dark";
  }
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
 * The rail's session order, mirroring the TUI sidebar (ui/sidebar_model.go): live
 * sessions first ordered oldest-created first, the archived group last ordered
 * newest-created first (#1605) — see compareSessionsForRail. Exported so keyboard
 * navigation (index.ts) walks the SAME order the DOM shows.
 */
export function orderedSessions(sessions: SessionData[]): SessionData[] {
  return [...sessions].sort(compareSessionsForRail);
}

/** Renders the paste-token login view, replacing the root's contents. */
export function renderLogin(root: HTMLElement, state: AppState, actions: Actions): void {
  root.replaceChildren(loginView(state, actions));
}

function loginView(state: AppState, actions: Actions): HTMLElement {
  // While the initial auth probe (or a silent token resume) is in flight, show a
  // neutral placeholder rather than flashing a paste-token field a tokenless daemon
  // may not need (#1696). An error always wins over the placeholder so a failed
  // probe/paste is surfaced.
  if (state.connecting && !state.loginError) {
    return connectingView();
  }
  // The daemon told us this client needs no token (loopback, or require_token=false,
  // #1696): skip the paste form entirely and offer a single tokenless Connect. The
  // empty-string token is the "no credential" sentinel the API layer understands.
  if (!state.authRequired) {
    return noAuthLoginView(state, actions);
  }
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
    // Say that the token is kept, and where the off switch is. Persisting a
    // full-access credential in the browser is the user's call to make knowingly —
    // silently writing it to disk is the thing not to do.
    h("p", { class: "af-subtitle af-login-note" }, "It stays saved in this browser until you disconnect."),
    form,
  ];
  if (state.loginError) {
    children.push(h("p", { class: "af-error", role: "alert" }, state.loginError));
  }

  return h("main", { class: "af-login" }, h("div", { class: "af-card" }, ...children));
}

/** The neutral "connecting" placeholder shown while the initial auth probe or a
 *  token resume is in flight (#1696), so no paste-token field flashes before we know
 *  whether this client even needs one. */
function connectingView(): HTMLElement {
  return h(
    "main",
    { class: "af-login" },
    h(
      "div",
      { class: "af-card" },
      h("h1", { class: "af-title" }, "Agent Factory"),
      h("p", { class: "af-subtitle" }, "Connecting…"),
    ),
  );
}

/** The tokenless login view (#1696): the daemon exempts this client, so there is no
 *  token to paste — just a Connect button that dials in with the empty-token
 *  sentinel. Normally auto-connected on load; this view is what a user sees only if
 *  they explicitly Disconnect on such a daemon. */
function noAuthLoginView(state: AppState, actions: Actions): HTMLElement {
  const button = h(
    "button",
    { type: "submit", class: "af-primary", disabled: state.connecting },
    state.connecting ? "Connecting…" : "Connect",
  );
  const form = h("form", { class: "af-login-form" }, button);
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    actions.connect("");
  });

  const children: (Node | string)[] = [
    h("h1", { class: "af-title" }, "Agent Factory"),
    h(
      "p",
      { class: "af-subtitle" },
      "This daemon does not require a token for your connection.",
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
  // A transient toast for failed tab ops (create/close), shown/hidden by `tabError`.
  private readonly toast: HTMLElement;

  // The top-level view switcher (#1592 Phase 5 PR8): the appbar tabs (by view) and
  // the three body surfaces they toggle between. The sessions body (rail+terminal)
  // stays mounted while another view shows — hidden, not destroyed — so switching
  // views never tears down the focused terminal or its scrollback.
  private readonly viewTabs = new Map<View, HTMLElement>();
  // The appbar theme toggle (redesign PR1): one button per Auto/Light/Dark choice,
  // the active one highlighted in update().
  private readonly themeOpts = new Map<ThemeChoice, HTMLElement>();
  private lastThemeChoice: ThemeChoice | null = null;
  private readonly sessionsBody: HTMLElement;
  private readonly tasksPane: TasksPane;
  private readonly configPane: ConfigPane;
  private lastView: View | null = null;
  private lastTasks: TaskData[] | null = null;
  private lastTasksProject: string | null = null;

  // The top-right project switcher (redesign PR2): a button showing the current
  // project + a dropdown menu listing every project with its per-project counts, the
  // single place project context changes. Managed imperatively (open/close, outside-
  // click dismiss) like the modals — its open state is UI ephemera, not store state.
  private readonly projectSwitchWrap: HTMLElement;
  private readonly projectSwitchBtn: HTMLButtonElement;
  private readonly projectSwitchName: HTMLElement;
  private readonly projectMenu: HTMLElement;
  private projectMenuOpen = false;
  // Change-detection for the switcher label + menu: rebuilt only when the session set,
  // the task set, or the selected project changes (the counts + the current-item
  // highlight; the task set can add/drop a task-only project).
  private lastProjectSessions: SessionData[] | null = null;
  private lastProjectTasks: TaskData[] | null = null;
  private lastSelectedProject: string | null = null;

  // The rail's status filter control (feat: hide archived by default): a rail-head
  // button + a checkbox menu, one row per session state. Same imperative treatment as
  // the project switcher above — its open state is UI ephemera, not store state, and
  // the checked set lives in the store (AppState.statusFilter).
  private readonly filterWrap: HTMLElement;
  private readonly filterBtn: HTMLButtonElement;
  private readonly filterMenu: HTMLElement;
  private readonly filterDot: HTMLElement;
  private filterMenuOpen = false;
  private lastStatusFilter: StatusFilter | null = null;

  // Header text nodes for the selected pane, (re)created per selection.
  private headTitle: HTMLElement | null = null;
  private headMeta: HTMLElement | null = null;
  // The selected rail row's archive/restore control and the daemon-owned verb it
  // currently shows (#1932, #2186, #2234). Every actionable row owns controls now
  // (#2223), but a session can flip archive⇄restore WITHOUT a selection change, so
  // patchMainHead still needs this reference to swap the selected control in place.
  // null when the selected row is not visible/actionable in the rail.
  private lifecycleBtn: HTMLElement | null = null;
  private lifecycleAction: LifecycleAction | null = null;
  // The usage-limit Retry button and whether it is currently shown (#1934). Same
  // treatment, and for the same reason, as lifecycleBtn above: a session hits the
  // limit wall — or is resumed off it — WITHOUT a selection change, which is the
  // only thing that rebuilds the header, so patchMainHead toggles it in place.
  private retryBtn: HTMLElement | null = null;
  private retryVisible = false;
  // The tab bar for the selected session, (re)created per selection and patched in
  // place when the tab list or active tab changes (#1592 Phase 5 PR7). null when
  // nothing is selected (the empty state has no tabs).
  private tabBar: HTMLElement | null = null;
  // The tab identities (kind:name) drawn in the bar at its last render, stamped into a
  // dragged tab's payload by the delegated dragstart so a drop can detect a mid-drag
  // tab-set change and cancel (see split.ts). Kept live by renderTabBar.
  private currentTabIds: string[] = [];
  // The REAL daemon tab ids drawn in the bar at its last render ("" for a tab with
  // none), parallel to currentTabIds. Separate because only a real id may be
  // treated as a stable identity by a drop (#1779) — see tabRealId.
  private currentTabRealIds: string[] = [];
  // A signature of everything the bar DRAWS (see tabBarSig): the bar is rebuilt only
  // when this changes, so an unrelated status snapshot never churns its DOM (#1737).
  private lastTabBarSig = "";
  // The insertion indicator for a reorder drag over the bar (#1813): a thin accent
  // rule drawn in the gap a drop would land in. It lives INSIDE the bar (which CSS
  // makes its containing block) and is re-appended after every rebuild, since
  // renderTabBar replaceChildren()es the bar's contents.
  private tabInsert: HTMLElement | null = null;
  // The tab index an in-flight drag from THIS bar started at, captured at dragstart
  // (#1813). dragover cannot read the payload — the dataTransfer is in protected
  // mode until the drop — so this is the only way the bar can know, WHILE the drag is
  // over it, whether the grabbed tab is the pinned agent tab and must refuse to
  // reorder. null when no tab drag from this bar is in flight (including a foreign
  // drag carrying the same MIME, which then falls through to the drop's own checks).
  private dragFromIndex: number | null = null;

  // Last-applied state, for cheap change detection between updates.
  private lastSessions: SessionData[] | null = null;
  private lastSelectedId: string | null = null;
  private lastLive: EventStreamStatus | null = null;
  private lastKb: KeyboardFocus | null = null;
  private lastError: string | null = null;
  // The last value written to document.title, so an unrelated update doesn't
  // reassign it (see syncDocumentTitle).
  private lastDocTitle: string | null = null;
  // Whether the main pane has been rendered at least once. The constructor leaves it
  // an empty <section>, so the FIRST update must render it even when nothing is
  // selected (selectedId is null before AND after that first update, so the
  // selection-changed guard alone wouldn't fire) — otherwise the pane is blank on
  // load until a select-then-deselect. (#1592 Phase 5 PR9)
  private mainRendered = false;

  // The narrow-viewport session rail (web mobile pass): below ~768px the rail is an
  // off-canvas drawer that the .af-nav-toggle hamburger slides over the terminal, so a
  // phone gives the terminal the full width. This is the ONLY piece of the responsive
  // work that needs JS — everything else is @media CSS. `navOpen` is pure UI ephemera
  // (never store state): the drawer is closed on load, opens on the toggle, and
  // auto-closes when an action leaves the rail (create/select/lifecycle) or the view
  // changes. Filtering stays in the rail and deliberately keeps it open (#2226).
  // On desktop the CSS ignores the class, so setNav is an inert no-op there.
  private readonly navToggle: HTMLButtonElement;
  private readonly navScrim: HTMLElement;
  private navOpen = false;

  constructor(
    private readonly actions: Actions,
    private readonly termHost: HTMLElement,
    private readonly modalHost: HTMLElement,
    /** The "Install app" affordance's root, mounted into the appbar (feat: PWA).
     *  Owned by index.ts rather than built here — it must outlive the shell, which is
     *  rebuilt on every logout/login. Optional so a caller that does not care about
     *  install (a harness) can leave it out. */
    private readonly installEl?: HTMLElement,
  ) {
    this.pip = h("span", { class: "af-live-pip" });
    this.pip.setAttribute("aria-hidden", "true");
    this.pipLabel = h("span", { class: "af-live-label" });
    const live = h("span", { class: "af-live" }, this.pip, this.pipLabel);
    live.setAttribute("role", "status");

    // Disconnect doubles as "forget the saved token": the credential persists across
    // visits now, so this button is the only way back to the login prompt on a shared
    // machine or after a rotation. The title says so — the label alone reads as a
    // transport action, not a logout.
    const disconnect = h("button", { type: "button", class: "af-ghost" }, "Disconnect");
    disconnect.setAttribute("title", "Disconnect and forget the saved token");

    // The theme toggle: a compact Auto/Light/Dark segmented control. A click routes
    // through actions.setTheme, which persists the choice and re-themes the terminals.
    const themeToggle = h("div", { class: "af-theme-toggle" });
    themeToggle.setAttribute("role", "group");
    themeToggle.setAttribute("aria-label", "Theme");
    for (const choice of THEME_CHOICES) {
      const opt = h("button", { type: "button", class: "af-theme-opt" }, themeLabel(choice));
      opt.setAttribute("data-theme-opt", choice);
      opt.setAttribute("title", `${themeLabel(choice)} theme`);
      opt.addEventListener("click", () => this.actions.setTheme(choice));
      this.themeOpts.set(choice, opt);
      themeToggle.append(opt);
    }

    // The view switcher: one tab per top-level view, left-to-right in the [ / ] cycle
    // order (nav.ts VIEWS), the active one highlighted in update(). A click routes
    // through actions.switchView, exactly like the keyboard path.
    const viewNav = h("div", { class: "af-viewnav" });
    viewNav.setAttribute("role", "tablist");
    viewNav.setAttribute("aria-label", "Views");
    for (const v of VIEWS) {
      const tab = h("button", { type: "button", class: "af-viewtab" }, viewLabel(v));
      tab.setAttribute("role", "tab");
      tab.setAttribute("data-view", v);
      tab.addEventListener("click", () => this.actions.switchView(v));
      this.viewTabs.set(v, tab);
      viewNav.append(tab);
    }

    // The project switcher (redesign PR2): a button showing the current project and a
    // dropdown listing every project with counts. `margin-left:auto` (the wrap) pushes
    // it — and everything after it — to the right of the segmented view nav.
    this.projectSwitchName = h("span", { class: "af-project-switch-name" }, "—");
    const switchGlyph = h("span", { class: "af-project-glyph" }, "▣");
    switchGlyph.setAttribute("aria-hidden", "true");
    const switchCaret = h("span", { class: "af-project-caret" }, "▼");
    switchCaret.setAttribute("aria-hidden", "true");
    this.projectSwitchBtn = h(
      "button",
      { type: "button", class: "af-project-switch" },
      switchGlyph,
      this.projectSwitchName,
      switchCaret,
    );
    this.projectSwitchBtn.setAttribute("aria-haspopup", "listbox");
    this.projectSwitchBtn.setAttribute("aria-expanded", "false");
    this.projectSwitchBtn.setAttribute("aria-label", "Switch project");
    this.projectSwitchBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      setAppbarToolsOpen(false);
      this.toggleProjectMenu();
    });
    this.projectMenu = h("div", { class: "af-project-menu" });
    this.projectMenu.setAttribute("role", "listbox");
    this.projectMenu.setAttribute("aria-label", "Switch project");
    this.projectMenu.hidden = true;
    this.projectSwitchWrap = h("div", { class: "af-project-switch-wrap" }, this.projectSwitchBtn, this.projectMenu);
    // Dismiss the menu on any click outside the switcher (a click on the button or a
    // menu item is handled by their own listeners, which stopPropagation / close).
    document.addEventListener("click", (e) => {
      if (this.projectMenuOpen && !this.projectSwitchWrap.contains(e.target as Node)) {
        this.closeProjectMenu();
      }
    });

    // The narrow-viewport rail toggle (web mobile pass): a hamburger that slides the
    // session drawer over the terminal. CSS keeps it display:none on desktop and shows
    // it only in the sessions view on a phone (the tasks/config views have no rail), so
    // it never competes with the appbar controls on a comfortable width.
    this.navToggle = h("button", { type: "button", class: "af-nav-toggle" }, "☰");
    this.navToggle.setAttribute("aria-label", "Toggle sessions");
    this.navToggle.setAttribute("aria-controls", "af-rail");
    this.navToggle.setAttribute("aria-expanded", "false");
    this.navToggle.addEventListener("click", () => this.toggleNav());

    // Secondary appbar chrome stays inline on desktop, but a phone cannot give all
    // four controls scarce primary-row width without clipping the project context
    // (#2227). Group them behind one More trigger at the narrow breakpoint instead
    // of deleting functionality or shrinking touch targets. The listeners exist only
    // while the popover is open, so logout/login cannot accumulate document handlers.
    const appbarTools = h(
      "div",
      { class: "af-appbar-tools", id: "af-appbar-tools" },
      live,
      ...(this.installEl ? [this.installEl] : []),
      themeToggle,
      disconnect,
    );
    appbarTools.setAttribute("role", "group");
    appbarTools.setAttribute("aria-label", "App controls");
    const appbarMore = h("button", { type: "button", class: "af-appbar-more" }, "⋯");
    appbarMore.setAttribute("aria-label", "More app controls");
    appbarMore.setAttribute("title", "More app controls");
    appbarMore.setAttribute("aria-controls", "af-appbar-tools");
    appbarMore.setAttribute("aria-expanded", "false");
    const appbarToolsWrap = h("div", { class: "af-appbar-tools-wrap" }, appbarMore, appbarTools);
    let appbarToolsOpen = false;
    function setAppbarToolsOpen(open: boolean): void {
      if (appbarToolsOpen === open) {
        return;
      }
      appbarToolsOpen = open;
      appbarToolsWrap.classList.toggle("af-appbar-tools-open", open);
      appbarMore.setAttribute("aria-expanded", open ? "true" : "false");
      if (open) {
        document.addEventListener("mousedown", onAppbarToolsMouseDown);
        document.addEventListener("keydown", onAppbarToolsKeyDown, true);
        window.addEventListener("resize", onAppbarToolsResize);
      } else {
        document.removeEventListener("mousedown", onAppbarToolsMouseDown);
        document.removeEventListener("keydown", onAppbarToolsKeyDown, true);
        window.removeEventListener("resize", onAppbarToolsResize);
      }
    }
    const onAppbarToolsMouseDown = (e: MouseEvent): void => {
      if (!appbarToolsWrap.isConnected || !appbarToolsWrap.contains(e.target as Node)) {
        setAppbarToolsOpen(false);
      }
    };
    const onAppbarToolsKeyDown = (e: KeyboardEvent): void => {
      if (e.key !== "Escape") {
        return;
      }
      e.stopPropagation();
      setAppbarToolsOpen(false);
      appbarMore.focus();
    };
    const onAppbarToolsResize = (): void => setAppbarToolsOpen(false);
    appbarMore.addEventListener("click", (e) => {
      e.stopPropagation();
      const opening = !appbarToolsOpen;
      if (opening) {
        this.closeProjectMenu();
      }
      setAppbarToolsOpen(opening);
    });
    // Disconnect replaces the shell synchronously. Close first so the temporary
    // document/window listeners cannot outlive the DOM subtree they describe.
    disconnect.addEventListener("click", () => {
      setAppbarToolsOpen(false);
      this.actions.disconnect();
    });

    const header = h(
      "header",
      { class: "af-appbar" },
      this.navToggle,
      h("span", { class: "af-brand" }, "Agent Factory"),
      viewNav,
      this.projectSwitchWrap,
      appbarToolsWrap,
    );

    this.railCount = h("span", { class: "af-rail-count" }, "0");
    const newBtn = h("button", { type: "button", class: "af-rail-new", title: "New session" }, "+ New");
    newBtn.addEventListener("click", () => this.runRailExit(() => this.actions.newSession()));

    // The status filter (feat: hide archived by default). A small funnel mark rather
    // than a word, so the rail head still fits the count + New at 18rem; the dot beside
    // it lights only when the filter is NARROWED from the default, so the normal state
    // (archived hidden) never nags. This is deliberately an inline SVG: Unicode has no
    // consistently rendered funnel, while currentColor preserves every button state.
    const filterGlyph = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    filterGlyph.classList.add("af-rail-filter-glyph");
    filterGlyph.setAttribute("viewBox", "0 0 16 16");
    filterGlyph.setAttribute("aria-hidden", "true");
    filterGlyph.setAttribute("focusable", "false");
    const filterGlyphPath = document.createElementNS("http://www.w3.org/2000/svg", "path");
    filterGlyphPath.setAttribute("d", "M2 2h12L9.5 8v4.5L6.5 14V8L2 2Z");
    filterGlyph.append(filterGlyphPath);
    this.filterDot = h("span", { class: "af-rail-filter-dot" });
    this.filterDot.setAttribute("aria-hidden", "true");
    this.filterBtn = h("button", { type: "button", class: "af-rail-filter" }, filterGlyph, this.filterDot);
    this.filterBtn.setAttribute("aria-haspopup", "true");
    this.filterBtn.setAttribute("aria-expanded", "false");
    this.filterBtn.setAttribute("aria-label", "Filter sessions");
    this.filterBtn.setAttribute("title", "Filter sessions");
    this.filterBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      this.toggleFilterMenu();
    });
    this.filterMenu = h("div", { class: "af-filter-menu" });
    this.filterMenu.setAttribute("role", "group");
    this.filterMenu.setAttribute("aria-label", "Filter sessions");
    this.filterMenu.hidden = true;
    this.filterWrap = h("div", { class: "af-rail-filter-wrap" }, this.filterBtn, this.filterMenu);
    // Dismiss on any click outside (the button + the checkboxes stopPropagation), so
    // the menu stays open across several toggles — the common act is narrowing to a
    // couple of states, not one click.
    document.addEventListener("click", (e) => {
      if (this.filterMenuOpen && !this.filterWrap.contains(e.target as Node)) {
        this.closeFilterMenu();
      }
    });

    const railHead = h(
      "div",
      { class: "af-rail-head" },
      h("span", { class: "af-rail-title" }, "Sessions"),
      this.railCount,
      h("div", { class: "af-rail-head-actions" }, this.filterWrap, newBtn),
    );
    this.railList = h("ul", { class: "af-rail-list" });
    this.railList.setAttribute("role", "listbox");
    this.railList.setAttribute("aria-label", "Sessions");
    const rail = h("nav", { class: "af-rail", id: "af-rail" }, railHead, this.railList);

    this.main = h("section", { class: "af-main" });
    // The drawer scrim: a tap-to-dismiss layer over the terminal while the mobile rail
    // is open. CSS keeps it display:none except in the narrow-viewport drawer state.
    this.navScrim = h("div", { class: "af-nav-scrim" });
    this.navScrim.setAttribute("aria-hidden", "true");
    this.navScrim.addEventListener("click", () => this.setNav(false));
    this.sessionsBody = h("div", { class: "af-body" }, rail, this.main, this.navScrim);

    // The tasks view is a peer of the sessions body inside one viewport; update()
    // shows exactly one and hides the other by `state.view`. It owns its own subtree
    // (scoped to the selected project) so a task.* event patches only that pane.
    this.tasksPane = new TasksPane({
      add: () => this.actions.addTask(),
      edit: (task: TaskData) => this.actions.editTask(task),
      toggle: (task: TaskData) => this.actions.toggleTask(task),
      trigger: (task: TaskData) => this.actions.triggerTask(task),
      remove: (task: TaskData) => this.actions.removeTask(task),
    });
    this.configPane = new ConfigPane({
      save: (key: string, value: string) => this.actions.setConfigValue(key, value),
    });
    const viewport = h("div", { class: "af-viewport" }, this.sessionsBody, this.tasksPane.el, this.configPane.el);

    // A transient toast for failed tab ops: a fixed-position banner that fades in
    // only while `tabError` is set (index.ts clears it on a timer / selection change).
    this.toast = h("div", { class: "af-toast" });
    this.toast.setAttribute("role", "alert");
    // The modal host is a persistent overlay layer index.ts mounts modals into; it
    // sits above the app body and is empty except while a modal is open.
    this.el = h("main", { class: "af-app" }, header, viewport, this.toast, this.modalHost);
  }

  /** Points the browser tab at what is on screen, so a pinned/backgrounded tab and the
   *  history entry name the session and project rather than a static "Agent Factory".
   *  Assigns only on a real change (a rename, a selection, or a project switch). */
  private syncDocumentTitle(state: AppState): void {
    const title = documentTitle(state);
    if (this.lastDocTitle !== title) {
      this.lastDocTitle = title;
      document.title = title;
    }
  }

  /** Opens or closes the narrow-viewport session drawer (web mobile pass). A class on
   *  the app root that the @media CSS turns into a slide-in rail + scrim; on desktop the
   *  CSS ignores it, so this is a no-op there. Cheap-guarded so a redundant call from
   *  update() doesn't thrash the class or the aria state. */
  private setNav(open: boolean): void {
    if (this.navOpen === open) {
      return;
    }
    this.navOpen = open;
    this.el.classList.toggle("af-nav-open", open);
    this.navToggle.setAttribute("aria-expanded", open ? "true" : "false");
  }

  /** Runs an action whose result lives outside the session drawer (#2226). Drawer
   *  dismissal belongs to the action's intent, not to where its click happened or
   *  whether it bubbled: Create and lifecycle controls stop at different DOM nodes,
   *  while both must expose the modal they open. Filter actions bypass this helper
   *  because their result remains inside the rail. */
  private runRailExit(action: () => void): void {
    this.setNav(false);
    action();
  }

  private toggleNav(): void {
    this.setNav(!this.navOpen);
  }

  /** Applies the latest state, touching only what changed. */
  update(state: AppState): void {
    this.syncDocumentTitle(state);
    // The keyboard-focus indicator (#1693): a modifier class on the app root that
    // CSS turns into an accent border on whichever pane owns the keyboard. The
    // terminal only "holds" it while a session is actually selected; with none
    // selected the main pane is the empty state, so the rail always reads as active.
    const kb: KeyboardFocus = state.selectedId && state.focus === "terminal" ? "terminal" : "rail";
    if (this.lastKb !== kb) {
      this.lastKb = kb;
      this.el.classList.toggle("af-kb-terminal", kb === "terminal");
      this.el.classList.toggle("af-kb-rail", kb === "rail");
    }

    if (this.lastError !== state.tabError) {
      this.lastError = state.tabError;
      this.toast.textContent = state.tabError ?? "";
      this.toast.classList.toggle("af-toast-show", state.tabError !== null);
    }

    if (this.lastLive !== state.live) {
      this.lastLive = state.live;
      this.pip.className = `af-live-pip af-live-${state.live}`;
      this.pipLabel.textContent =
        state.live === "open" ? "Live" : state.live === "connecting" ? "Connecting…" : "Reconnecting…";
    }

    // View switching: show the active view's body, hide the other, and highlight its
    // appbar tab. The sessions body is only HIDDEN (never removed), so the terminal
    // host inside it — and its focus/scrollback — survives a round trip to the tasks
    // view.
    if (this.lastView !== state.view) {
      this.lastView = state.view;
      this.sessionsBody.hidden = state.view !== "sessions";
      this.tasksPane.el.hidden = state.view !== "tasks";
      this.configPane.el.hidden = state.view !== "config";
      for (const [v, tab] of this.viewTabs) {
        tab.classList.toggle("af-viewtab-active", v === state.view);
        tab.setAttribute("aria-selected", v === state.view ? "true" : "false");
      }
      // The mobile rail drawer belongs to the sessions view (tasks/config have no
      // rail): gate the hamburger's visibility on a root class, and fold the drawer
      // shut on any view switch so it never lingers open over another surface.
      this.el.classList.toggle("af-view-sessions", state.view === "sessions");
      this.setNav(false);
    }

    // Highlight the active theme option (redesign PR1) when the choice changes.
    if (this.lastThemeChoice !== state.themeChoice) {
      this.lastThemeChoice = state.themeChoice;
      for (const [choice, opt] of this.themeOpts) {
        const active = choice === state.themeChoice;
        opt.classList.toggle("af-theme-opt-active", active);
        opt.setAttribute("aria-pressed", active ? "true" : "false");
      }
    }

    // The tasks pane mirrors the task projection, SCOPED to the selected project
    // (redesign PR2). It re-renders when either the task list or the project scope
    // changes, so switching projects re-scopes the tasks view too.
    if (this.lastTasks !== state.tasks || this.lastTasksProject !== state.selectedProject) {
      this.lastTasks = state.tasks;
      this.lastTasksProject = state.selectedProject;
      this.tasksPane.update(state.tasks, state.selectedProject);
    }

    // The config pane mirrors the manifest. Global config is NOT project-scoped —
    // config.toml applies to every repo — so unlike the tasks pane it re-renders on
    // the data alone, with no project in the change check.
    this.configPane.update(state.config, state.configPath, state.configStatus);

    const sessionsChanged = this.lastSessions !== state.sessions;
    const selectionChanged = this.lastSelectedId !== state.selectedId;
    const projectChanged = this.lastSelectedProject !== state.selectedProject;

    // Selecting a session on a phone should reveal its terminal, so fold the drawer
    // shut whenever the selection lands on a real session — the keyboard/programmatic
    // path (j/k then Enter) that never touches the rail's delegated click. Guarded on a
    // non-null id so a deselection doesn't yank a browsing user out of the rail.
    if (selectionChanged && state.selectedId) {
      this.setNav(false);
    }

    // The project switcher label + menu reflect the derived project list and the
    // current scope; rebuild when the sessions, the tasks (a task-only project), or
    // the selection changed.
    if (this.lastProjectSessions !== state.sessions || this.lastProjectTasks !== state.tasks || projectChanged) {
      this.lastProjectSessions = state.sessions;
      this.lastProjectTasks = state.tasks;
      this.lastSelectedProject = state.selectedProject;
      this.renderProjectSwitch(state);
    }

    this.lastSessions = state.sessions;
    this.lastSelectedId = state.selectedId;

    // Rebuild the rail when the list, the highlighted row, the project scope, OR the
    // status filter changed (each of the four changes which rows show). None touches
    // the terminal host (it lives in the main pane), so events never blur it.
    const filterChanged = this.lastStatusFilter !== state.statusFilter;
    this.lastStatusFilter = state.statusFilter;
    if (sessionsChanged || selectionChanged || projectChanged || filterChanged) {
      this.renderRail(state);
    }

    // The main pane's STRUCTURE only changes when the selected session changes (or on
    // the very first update, which lays down the initial empty-state placeholder);
    // otherwise we just patch its header text (status/title/branch), leaving the
    // terminal host — and its focus and scrollback — in place.
    if (selectionChanged || !this.mainRendered) {
      this.mainRendered = true;
      this.renderMain(state);
    } else {
      this.patchMainHead(state);
      // The tab bar reflects the live tab list and the active/shown highlight; any of
      // those can change without a selection change (a resync grows/shrinks the list, a
      // 1-9 key moves the highlight, a pane split changes the shown set). Rebuild ONLY
      // when that signature actually changes — NOT on every sessions snapshot: an
      // unrelated status-only event (a rail update) must not replaceChildren() the bar,
      // because that destroys the very button a user has grabbed to drag, breaking a
      // real HTML5 drag mid-gesture on a freshly-created tab (#1737 follow-up). termHost
      // (and its focused xterm) is untouched either way.
      if (tabBarSig(state) !== this.lastTabBarSig) {
        this.renderTabBar(state);
      }
    }

    // The dragstart caches are refreshed on EVERY snapshot, deliberately outside the
    // tab-bar's render gate (#1779). The gate keys on tabBarSig, which covers only
    // what the bar DRAWS — kind/name/active/shown — and so ignores tab IDs. That is
    // correct for the DOM (an id backfill changes no pixel, and rebuilding the bar
    // would destroy a button mid-drag, the #1737 regression) but wrong for the
    // caches: a just-created tab whose id the next snapshot backfills would keep a
    // stale "" here, and the drag it stamps would fall back to the legacy guard
    // instead of using the now-known stable id. Adding ids to the signature would fix
    // the cache by reintroducing exactly the #1737 rebuild — so the cache is synced
    // independently of the render instead.
    this.syncTabIdentityCaches(state);
  }

  /** Refreshes the ordered tab identity + REAL-id caches the delegated dragstart
   *  stamps into a payload. Cheap (two maps over ≤9 tabs) and pure bookkeeping — it
   *  touches no DOM, so it is safe to run on every snapshot. */
  private syncTabIdentityCaches(state: AppState): void {
    const selected = selectedSession(state);
    const tabs = selected ? sessionTabs(selected) : [];
    this.currentTabIds = tabs.map(tabIdentity);
    this.currentTabRealIds = tabs.map(tabRealId);
  }

  /** The CURRENT identity of the tab drawn at `index`, for a GESTURE fired from a
   *  button that may have been built against an older snapshot.
   *
   *  A tab button outlives every snapshot that leaves tabBarSig unchanged, so the tab
   *  object it closed over can go stale in the one way the signature cannot see: its
   *  IDENTITY. Reading the live cache instead means a gesture names the tab by what it
   *  is NOW, not by what it was when its button was drawn — the same reason the drag
   *  payload reads these caches rather than the render (#1779).
   *
   *  Read WHEN A GESTURE FIRES, and for a gesture with duration that means when it
   *  BEGINS — dragstart stamps its payload here, and a rename captures its subject here
   *  as the edit opens (beginTabRename). Deliberately not re-read when such a gesture
   *  COMMITS: by then this answers "who is at this position now?", which a close+recreate
   *  of the same name makes a different tab, invisibly to the signature.
   *
   *  The index is not stale in the same way, and that is structural rather than lucky:
   *  the signature pins the whole ORDERED kind/name list, so any snapshot that could
   *  move a tab to a different ordinal necessarily rebuilds the bar and replaces this
   *  button. A surviving button therefore still sits at its own index.
   *
   *  "" (matching no tab, so the action refuses rather than guesses) if the index is
   *  somehow past the roster — the same honest no-answer tabRealId returns. */
  private liveTabIdentity(index: number): string {
    return this.currentTabIds[index] ?? "";
  }

  /** Renders the rail SCOPED to the selected project (redesign PR2) and FILTERED by
   *  the status filter (feat: hide archived by default): only that project's sessions
   *  in a state the user asked to see, never the whole projection. No project at all
   *  (nothing created yet) → the "no sessions yet" empty rail; otherwise the visible
   *  rows, under a notice when there is something worth saying about what's missing. */
  private renderRail(state: AppState): void {
    const scoped = scopeToProject(state.sessions, state.selectedProject);
    const visible = filterSessions(orderedSessions(scoped), state.statusFilter);
    // Every rebuild replaces the row controls. Drop the old reference first so a
    // selected row hidden by the project/status filter cannot leave patchMainHead
    // mutating a detached button.
    this.lifecycleBtn = null;
    this.lifecycleAction = null;
    // The count is what the rail SHOWS, not what the project holds — a count that
    // disagrees with the rows under it is just a bug the user has to reconcile. The
    // filter menu carries the per-state totals for what's hidden.
    this.railCount.textContent = String(visible.length);
    this.renderFilterMenu(state, scoped);
    const list = this.railList;
    // No project selected ⇒ there are no projects at all (nothing has been created):
    // the global empty rail, its copy pointing at how to create the first session.
    if (!state.selectedProject) {
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
    const rows = visible.map((s) => {
      const selected = s.id === state.selectedId;
      return sessionRow(
        s,
        selected,
        (id) => this.runRailExit(() => this.actions.open(id)),
        (target) => this.rowActions(target, selected),
      );
    });
    const notice = this.railNotice(state, scoped, visible);
    list.replaceChildren(...(notice ? [notice, ...rows] : rows));
  }

  /** Quiet per-session controls reserved beside every ACTIONABLE rail row (#2186,
   *  #2223, #2234). The Go projection chooses Archive vs Restore; the browser never
   *  reconstructs that policy from row state. */
  private rowActions(session: ActionableSession, selected: boolean): HTMLElement {
    const lifecycleBtn = h("button", { type: "button", class: "af-rail-action af-rail-lifecycle" });
    lifecycleBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      if (lifecycleBtn.dataset.action === "restore") {
        this.runRailExit(() => this.actions.restore(session));
      } else {
        this.runRailExit(() => this.actions.archive(session));
      }
    });
    this.patchLifecycleButton(lifecycleBtn, session.lifecycle_action, session.title);
    if (selected) {
      this.lifecycleBtn = lifecycleBtn;
      this.lifecycleAction = session.lifecycle_action;
    }

    // Kill stays unmistakably destructive through its distinct ⌫ glyph and confirm,
    // but its resting rail treatment is deliberately muted instead of af-danger.
    const killBtn = h("button", { type: "button", class: "af-rail-action af-rail-kill" }, "⌫");
    const killLabel = `Kill session “${session.title}”`;
    killBtn.setAttribute("aria-label", killLabel);
    killBtn.setAttribute("title", killLabel);
    killBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      this.runRailExit(() => this.actions.kill(session));
    });
    return h("div", { class: "af-row-actions" }, lifecycleBtn, killBtn);
  }

  /** Applies the daemon-projected verb, glyph, and target-qualified accessible name
   *  in one place so render and same-selection live patching cannot drift. */
  private patchLifecycleButton(btn: HTMLElement, action: LifecycleAction, sessionTitle: string): void {
    const verb = action === "restore" ? "Restore session" : "Archive session";
    const label = `${verb} “${sessionTitle}”`;
    btn.dataset.action = action;
    btn.textContent = action === "restore" ? "↶" : "▪";
    btn.setAttribute("aria-label", label);
    btn.setAttribute("title", label);
  }

  /**
   * The dim one-liner above the rows explaining what ISN'T there, or null when the
   * rail speaks for itself. Three things are worth saying, in precedence order:
   *
   *  - the project has no ACTIVE session — the redesign PR2 empty state, now
   *    accurate about the archive: with archived hidden it names the count sitting
   *    behind the filter (never silently missing), and offers to reveal it in place.
   *    It renders ABOVE any archived rows, so "no active sessions" reads as a
   *    statement about live work, not a contradiction of the rows below.
   *  - every row is filtered out by a narrowed filter — an honest "nothing matches"
   *    with the way back, rather than a rail that looks broken.
   *  - otherwise nothing: rows are showing.
   */
  private railNotice(state: AppState, scoped: SessionData[], visible: SessionData[]): HTMLElement | null {
    const name = projectName(state.selectedProject ?? "");
    const hasActive = scoped.some((s) => !isArchived(s));
    if (!hasActive) {
      const newBtn = h("button", { type: "button", class: "af-rail-empty-new", title: "New session" }, "+ New");
      newBtn.addEventListener("click", () => this.runRailExit(() => this.actions.newSession()));
      const empty = h("li", { class: "af-rail-empty-project" }, `No active sessions in ${name} — `, newBtn);
      const archived = scoped.filter(isArchived).length;
      if (archived > 0 && !state.statusFilter.archived) {
        const show = h("button", { type: "button", class: "af-rail-empty-new af-rail-show-archived" }, "Show archived");
        show.addEventListener("click", () => this.actions.setStatusFilter("archived", true));
        empty.append(` · ${archived} archived hidden `, show);
      }
      return empty;
    }
    if (visible.length === 0) {
      const reset = h("button", { type: "button", class: "af-rail-empty-new af-rail-reset-filter" }, "Reset filter");
      reset.addEventListener("click", () => this.actions.resetStatusFilter());
      return h(
        "li",
        { class: "af-rail-empty-project" },
        `No sessions match the filter — ${hiddenCount(scoped, state.statusFilter)} hidden `,
        reset,
      );
    }
    return null;
  }

  /** (Re)builds the filter menu: one checkbox per session state with its scoped
   *  count, plus a reset. Counts come from the PROJECT-scoped list (the filter is a
   *  rail control, and the rail is single-project), so they always describe the rows
   *  the menu governs. Rebuilt in place — the menu's open state is preserved across
   *  rebuilds so a live event never snaps it shut mid-narrowing. */
  private renderFilterMenu(state: AppState, scoped: SessionData[]): void {
    const counts = kindCounts(scoped);
    const narrowed = !isDefaultFilter(state.statusFilter);
    this.filterDot.classList.toggle("af-rail-filter-dot-on", narrowed);
    this.filterBtn.classList.toggle("af-rail-filter-narrowed", narrowed);
    const children: HTMLElement[] = [h("div", { class: "af-filter-menu-label" }, "Show sessions")];
    for (const kind of FILTER_KINDS) {
      children.push(this.filterItem(kind, state.statusFilter[kind], counts[kind]));
    }
    const reset = h("button", { type: "button", class: "af-ghost af-filter-reset" }, "Reset to default");
    reset.disabled = !narrowed;
    reset.addEventListener("click", (e) => {
      e.stopPropagation();
      this.actions.resetStatusFilter();
    });
    children.push(h("div", { class: "af-filter-menu-foot" }, reset));
    this.filterMenu.replaceChildren(...children);
  }

  /** One state's checkbox in the filter menu: a check when shown, the state's label
   *  (the row's own word — status.ts ROW_KIND_LABELS), and how many sessions in this
   *  project are in it. Clicking toggles just that state and leaves the menu open. */
  private filterItem(kind: RowKind, on: boolean, count: number): HTMLElement {
    const check = h("span", { class: "af-filter-check" }, on ? "✓" : "");
    check.setAttribute("aria-hidden", "true");
    const item = h(
      "button",
      { type: "button", class: `af-filter-item${on ? " af-filter-item-on" : ""}` },
      check,
      h("span", { class: "af-filter-item-label" }, filterLabel(kind)),
      h("span", { class: "af-filter-item-count" }, String(count)),
    );
    item.dataset.kind = kind;
    item.setAttribute("role", "checkbox");
    item.setAttribute("aria-checked", on ? "true" : "false");
    item.addEventListener("click", (e) => {
      e.stopPropagation();
      this.actions.setStatusFilter(kind, !on);
    });
    return item;
  }

  /** (Re)builds the project switcher: the button's current-project name and the
   *  dropdown menu of every project with its per-project counts (redesign PR2). The
   *  menu's open/closed state (`hidden`) is preserved across rebuilds so a rebuild
   *  triggered by a live event doesn't snap an open menu shut. */
  private renderProjectSwitch(state: AppState): void {
    const summaries = projectSummaries(state.sessions, state.tasks);
    const current = state.selectedProject;
    this.projectSwitchName.textContent = current ? projectName(current) : "No project";
    // Disable the switcher when there are no projects to switch between.
    this.projectSwitchBtn.disabled = summaries.length === 0;

    const children: HTMLElement[] = [h("div", { class: "af-project-menu-label" }, "Switch project")];
    if (summaries.length === 0) {
      children.push(h("div", { class: "af-project-menu-empty" }, "No projects yet."));
    }
    for (const p of summaries) {
      children.push(this.projectItem(p, p.root === current));
    }
    // The reversible delete-project control (#1735) lived on the old Projects view's
    // header; with that view folded into the switcher it moves here, as a footer
    // action on the CURRENT project (single-project IA). index.ts pops the confirm.
    const currentSummary = summaries.find((p) => p.root === current);
    if (currentSummary) {
      const del = h("button", { type: "button", class: "af-ghost af-project-delete" }, "Delete project");
      // Delete-project ARCHIVES the project's live sessions (#1735, reversible). With
      // no live sessions to archive it would be a silent no-op, so it is DISABLED for a
      // task-only project — its tasks are cleared from the Tasks view, not here. This is
      // also why an archived-only repo is never a project (projectSummaries): the delete
      // can never appear-but-do-nothing.
      if (currentSummary.liveCount === 0) {
        del.disabled = true;
        del.setAttribute(
          "title",
          `No live sessions in ${currentSummary.name} to archive — remove its tasks from the Tasks view to clear it`,
        );
      } else {
        del.setAttribute("title", `Delete project ${currentSummary.name} (archives its sessions, restorable)`);
        del.addEventListener("click", (e) => {
          e.stopPropagation();
          this.closeProjectMenu();
          this.actions.deleteProject(currentSummary.root, currentSummary.name, currentSummary.liveCount);
        });
      }
      children.push(h("div", { class: "af-project-menu-foot" }, del));
    }
    this.projectMenu.replaceChildren(...children);
  }

  /** One project row in the switcher menu: a check on the current project, the name +
   *  full path, and the cross-project glance (session + working counts). Clicking it
   *  switches the active project and closes the menu. */
  private projectItem(p: ProjectSummary, current: boolean): HTMLElement {
    const cls = `af-project-item${current ? " af-project-item-current" : ""}`;
    const check = h("span", { class: "af-project-check" }, current ? "✓" : "");
    check.setAttribute("aria-hidden", "true");
    const label = h(
      "span",
      { class: "af-project-item-label" },
      h("span", { class: "af-project-item-name" }, p.name),
      h("span", { class: "af-project-item-path" }, p.path),
    );
    const meta = h("span", { class: "af-project-item-meta" }, projectMeta(p));
    const item = h("button", { type: "button", class: cls }, check, label, meta);
    item.setAttribute("role", "option");
    item.setAttribute("aria-selected", current ? "true" : "false");
    item.addEventListener("click", (e) => {
      e.stopPropagation();
      this.closeProjectMenu();
      this.actions.switchProject(p.root);
    });
    return item;
  }

  /** The visible `+ New tab` button and its kind menu.
   *
   *  The old split control created a terminal from `+` and hid VS Code behind a
   *  separate, unlabeled `▾`. Even the project's maintainer could not find that
   *  path (#2077), so the labelled button now makes the choice explicit where the
   *  editor will appear. The `t` shortcut remains the direct shell fast path.
   *
   *  Built per render (the tab bar is rebuilt wholesale), so the menu's listeners
   *  are bound to THIS instance and torn down with it — see the isConnected check
   *  in onDocMouseDown, which self-cleans if a rerender detaches an open menu. */
  private newTabControl(): HTMLElement {
    const wrap = h("div", { class: "af-tab-new-wrap" });
    const trigger = h(
      "button",
      { type: "button", class: "af-tab-new", title: "Create a terminal or VS Code tab" },
      h("span", { class: "af-tab-new-plus" }, "+"),
      h("span", {}, "New tab"),
      h("span", { class: "af-tab-new-caret" }, "▾"),
    );
    trigger.setAttribute("aria-haspopup", "menu");
    trigger.setAttribute("aria-expanded", "false");
    trigger.setAttribute("aria-label", "New tab · Terminal or VS Code");
    const menu = h("div", { class: "af-tab-menu" });
    menu.setAttribute("role", "menu");
    menu.setAttribute("aria-label", "Tab type");
    menu.hidden = true;

    let scrollParent: HTMLElement | null = null;
    const positionMenu = (): void => {
      // The menu is fixed so the tab bar's horizontal overflow cannot clip it. Anchor
      // its right edge to the trigger's caret, then keep the whole box in the viewport.
      const anchor = trigger.getBoundingClientRect();
      const menuBox = menu.getBoundingClientRect();
      const maxLeft = Math.max(0, window.innerWidth - menuBox.width);
      const left = Math.min(Math.max(0, anchor.right - menuBox.width), maxLeft);
      const below = anchor.bottom + 6;
      const above = anchor.top - menuBox.height - 6;
      const top = below + menuBox.height <= window.innerHeight ? below : Math.max(0, above);
      menu.style.left = `${left}px`;
      menu.style.top = `${top}px`;
    };
    const close = (): void => {
      menu.hidden = true;
      trigger.setAttribute("aria-expanded", "false");
      document.removeEventListener("mousedown", onDocMouseDown);
      document.removeEventListener("keydown", onKeyDown, true);
      scrollParent?.removeEventListener("scroll", positionMenu);
      scrollParent = null;
      window.removeEventListener("resize", positionMenu);
    };
    const onDocMouseDown = (e: MouseEvent): void => {
      // A rerender can detach this control while the menu is open; closing on a
      // detached wrap is what unregisters these document listeners, so they can
      // never accumulate across renders.
      if (!wrap.isConnected || !wrap.contains(e.target as Node)) {
        close();
      }
    };
    const onKeyDown = (e: KeyboardEvent): void => {
      if (e.key !== "Escape") {
        return;
      }
      // Swallow it: an open menu owns Escape, so closing it must not ALSO detach
      // the terminal / drop rail focus the way a bare Escape does.
      e.stopPropagation();
      close();
      trigger.focus();
    };
    const open = (): void => {
      menu.hidden = false;
      positionMenu();
      trigger.setAttribute("aria-expanded", "true");
      scrollParent = wrap.closest<HTMLElement>(".af-tabbar");
      scrollParent?.addEventListener("scroll", positionMenu, { passive: true });
      window.addEventListener("resize", positionMenu);
      document.addEventListener("mousedown", onDocMouseDown);
      // CAPTURE phase, deliberately. The app's own keydown handler is a
      // capture-phase listener on document that stopPropagation()s Escape (so
      // detaching can't leak a stray ESC into the PTY), which stops the event
      // before it can bubble back here — a bubble-phase listener would simply never
      // see it, and Escape would not close this menu. Same-node listeners are
      // unaffected by stopPropagation, so registering in capture puts this beside
      // the app's handler rather than downstream of it.
      document.addEventListener("keydown", onKeyDown, true);
    };

    const item = (label: string, kind: NewTabKind): HTMLElement => {
      const b = h("button", { type: "button", class: "af-tab-menu-item" }, label);
      b.setAttribute("role", "menuitem");
      b.addEventListener("click", (e) => {
        e.stopPropagation();
        close();
        this.actions.newTab(kind);
      });
      return b;
    };
    menu.append(item("Terminal", "shell"), item("VS Code", "vscode"));

    trigger.addEventListener("click", (e) => {
      e.stopPropagation();
      if (menu.hidden) {
        open();
      } else {
        close();
      }
    });
    wrap.append(trigger, menu);
    return wrap;
  }

  private toggleFilterMenu(): void {
    this.filterMenuOpen = !this.filterMenuOpen;
    this.filterMenu.hidden = !this.filterMenuOpen;
    this.filterBtn.setAttribute("aria-expanded", this.filterMenuOpen ? "true" : "false");
  }

  private closeFilterMenu(): void {
    if (!this.filterMenuOpen) {
      return;
    }
    this.filterMenuOpen = false;
    this.filterMenu.hidden = true;
    this.filterBtn.setAttribute("aria-expanded", "false");
  }

  private toggleProjectMenu(): void {
    if (this.projectMenuOpen) {
      this.closeProjectMenu();
    } else {
      this.openProjectMenu();
    }
  }

  private openProjectMenu(): void {
    if (this.projectSwitchBtn.disabled) {
      return;
    }
    this.projectMenuOpen = true;
    this.projectMenu.hidden = false;
    this.projectSwitchBtn.setAttribute("aria-expanded", "true");
  }

  private closeProjectMenu(): void {
    if (!this.projectMenuOpen) {
      return;
    }
    this.projectMenuOpen = false;
    this.projectMenu.hidden = true;
    this.projectSwitchBtn.setAttribute("aria-expanded", "false");
  }

  private renderMain(state: AppState): void {
    const selected = selectedSession(state);
    if (!selected) {
      this.headTitle = null;
      this.headMeta = null;
      this.retryBtn = null;
      this.retryVisible = false;
      this.tabBar = null;
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
    const titleBox = h("div", { class: "af-term-head-main" }, this.headTitle, this.headMeta);

    // Retry, for a session parked at a usage-limit wall (#1934). The web rendered
    // that state — ◆ glyph, "Limit reached" label, "[limit] resets …" title prefix
    // — and offered nothing to do about it, so the session sat until someone found
    // a terminal and opened the TUI.
    //
    // Shown only while the selection is limit-blocked, mirroring the TUI, which
    // advertises `c` only for a limit-blocked row (ui/menu.go) rather than showing
    // a dead control on every session.
    //
    // Hidden via `hidden`, and patched by patchMainHead — NOT decided once here.
    // renderMain runs only on a SELECTION change, and the common path is a session
    // hitting the wall while it is already selected, which is no selection change
    // at all (#1932, the same trap the archive/restore verb hit). A render-time
    // decision would mean the button appears only if you look away and back.
    const retryBtn = h("button", { type: "button", class: "af-ghost af-term-action" }, "Retry");
    retryBtn.title = "Resume this session from its usage-limit wall";
    retryBtn.addEventListener("click", () => this.actions.retryLimit());
    this.retryBtn = retryBtn;
    this.retryVisible = isLimitReached(selected);
    retryBtn.hidden = !this.retryVisible;

    // The tab bar is the flexible middle of the single pane-header row (#2224):
    // title first, the same horizontally scrolling bar, then the fixed Retry escape
    // when a limit wall makes it visible. Keeping the real bar node here (rather
    // than projecting a second mobile/desktop copy) preserves one drag/drop and
    // popover-anchoring path at every width.
    const tabBar = h("div", { class: "af-tabbar" });
    this.tabBar = tabBar;
    tabBar.setAttribute("role", "tablist");
    tabBar.setAttribute("aria-label", "Session tabs");
    // The drag source is wired ONCE here on the (stable) bar container via delegation,
    // not per button — so EVERY tab, including one created after load, is a drag source
    // by construction, with no per-button binding to forget on a re-render (#1737).
    this.attachTabDrag(tabBar);
    // ...and the bar is also a drop TARGET, for reordering (#1813). Same delegation,
    // same reason. See attachTabReorder for how this stays unambiguous against the
    // drag-to-split drop the panes wire.
    this.tabInsert = h("div", { class: "af-tab-insert" });
    this.tabInsert.hidden = true;
    this.tabInsert.setAttribute("aria-hidden", "true");
    this.attachTabReorder(tabBar);
    // Rename is delegated from the same stable container. The first click of a
    // double-click can activate an inactive tab and synchronously replace every
    // button; a listener owned by the old button cannot receive the final dblclick.
    this.attachTabRename(tabBar);

    // Retry is the only pane-level action. Append it directly instead of keeping a
    // wrapper box: hidden controls create no flex item and therefore no phantom gap
    // on the common path, while the visible button cannot shrink behind the tabs.
    const head = h("div", { class: "af-term-head" }, titleBox, tabBar, retryBtn);

    this.main.className = "af-main af-main-term";
    // The persistent terminal host is (re)mounted here; renderMain runs only on a
    // selection change, so this reparent is rare and never happens mid-type.
    this.main.replaceChildren(head, this.termHost);
    this.renderTabBar(state);
    this.patchMainHead(state);
  }

  /**
   * Ends an inline tab rename that is open in the bar, through the edit's OWN blur
   * path — the same one a click elsewhere takes, so Enter/blur/Escape keep their
   * single meaning and this adds no fourth way to leave an edit.
   *
   * Called by renderTabBar before it rebuilds; see the note there for why the rebuild
   * must not be the thing that evicts the input. Deliberately a blur() rather than a
   * direct remove: the edit owns whether an exit commits, and blurring lets it decide
   * exactly as it would for a click away.
   */
  private settleTabEdit(): void {
    // Scoped to the bar's own edit, and only while it holds focus — an unfocused input
    // has already been settled (Enter/Escape restore the button before their blur
    // lands), so there is nothing to end.
    const edit = this.tabBar?.querySelector<HTMLElement>(".af-tab-edit");
    if (edit && document.activeElement === edit) {
      edit.blur();
    }
  }

  /** (Re)builds the tab bar's buttons from the selected session's tabs, with the
   *  active tab highlighted. Rebuilds only the bar's children, so the sibling
   *  termHost — and the focused xterm in it — is never touched. */
  private renderTabBar(state: AppState): void {
    const bar = this.tabBar;
    if (!bar) {
      return;
    }
    const selected = selectedSession(state);
    if (!selected) {
      return;
    }
    // Settle an open inline rename BEFORE rebuilding, rather than letting
    // replaceChildren() evict its input. The input is a CHILD of this bar, and Chromium
    // fires the blur of a focused node it is removing while that node is STILL
    // connected — so the blur handler's own DOM restore (input.replaceWith(btn)) lands
    // in the middle of the child list replaceChildren is walking, and replaceChildren
    // then throws NotFoundError on a node that "is no longer a child". That aborts the
    // repaint half-done, leaving a bar holding stale buttons and no fresh ones. Blurring
    // here runs the identical commit path with the list quiescent, so the rebuild below
    // starts from a bar that only holds buttons. A no-op when no edit is open, and
    // (`done`) when the edit was already settled by Enter/Escape.
    this.settleTabEdit();
    const tabs = sessionTabs(selected);
    const canManage = canManageTabs(selected);
    // The active index is clamped: a resync that shrank the list must not leave the
    // highlight (and the streamed tab) pointing past the end.
    const active = Math.min(Math.max(state.activeTab, 0), tabs.length - 1);
    // Which tabs are currently rendered in a pane (feat: split tabs), so the bar can
    // mark an already-open tab distinctly from the focused one.
    const shown = new Set(state.shownTabs);
    // The dragstart caches are NOT refreshed here: this render is gated on tabBarSig,
    // which ignores tab ids, so an id backfill would never reach them. update() syncs
    // them on every snapshot instead — see syncTabIdentityCaches (#1779).

    const children: HTMLElement[] = tabs.map((tab, i) =>
      tabButton(tab, i, i === active, shown.has(i), canManage, this.actions, () => this.liveTabIdentity(i), selected.id ?? ""),
    );
    const unavailable = tabCreationUnavailableReason(selected, tabs.length);
    if (unavailable === null) {
      children.push(this.newTabControl());
    } else {
      const reason = h("span", { class: "af-tab-new-unavailable", title: unavailable }, unavailable);
      reason.setAttribute("aria-label", `New tab unavailable · ${unavailable}`);
      children.push(reason);
    }
    // Replacing every child resets a horizontally scrolled bar to its left edge.
    // Preserve the stable container's viewport so activating an off-screen tab does
    // not snap the row — or move a different tab under the second half of a gesture.
    const scrollLeft = bar.scrollLeft;
    bar.replaceChildren(...children);
    // The insertion indicator is absolutely positioned and sits outside the tab flow,
    // so it simply rides along as a last child — but replaceChildren above just
    // detached it, so it has to go back. Re-hidden with it: the rebuild that dropped
    // it may well be the one that ENDED the drag it was drawn for.
    if (this.tabInsert) {
      this.tabInsert.hidden = true;
      bar.append(this.tabInsert);
    }
    bar.scrollLeft = scrollLeft;
    // Rebuilding the bar detaches EVERY tab button — including the source of an
    // in-flight drag. A native dragend can't fire on a detached node, so the delegated
    // dragend (on the bar) would never run and the drag flag would stick, leaving the UI
    // in "dragging" mode (pane hints + drop overlay, both gated on the flag). Clear it
    // here so a drag that loses its source still ends cleanly. A no-op when idle.
    document.body.classList.remove("af-dragging-tab");
    this.dragFromIndex = null;
    // Record the signature this render represents, so update() skips a rebuild until
    // the bar's inputs actually change again.
    this.lastTabBarSig = tabBarSig(state);
  }

  /** Wires the tab bar as a DRAG SOURCE via event DELEGATION on the (stable) bar
   *  container. Binding once here — rather than per button in tabButton — means every
   *  tab is a drag source no matter when it was created, and a bar re-render can't drop
   *  the wiring for a subset of tabs (#1737 follow-up). dragstart reads the grabbed
   *  tab's index from its data-tab-index and stamps the payload: the index PLUS a
   *  snapshot of the live tab identities, so the drop cancels if the tab set changed
   *  mid-drag (split.ts). The body flag lets panes show drop hints. */
  private attachTabDrag(bar: HTMLElement): void {
    bar.addEventListener("dragstart", (e) => {
      const btn = (e.target as HTMLElement | null)?.closest<HTMLElement>(".af-tab");
      if (!btn || !bar.contains(btn) || !e.dataTransfer) {
        return;
      }
      const index = Number(btn.dataset.tabIndex);
      if (!Number.isInteger(index)) {
        return;
      }
      // Stamp the dragged tab's STABLE id (#1738) so the drop resolves it to the
      // tab's CURRENT ordinal — correct even if a concurrent client reordered/closed
      // tabs mid-drag. index + the identity snapshot ride along as a legacy fallback
      // for a tab without an id (pre-#1738 record). The id comes from the REAL-id
      // list, so a tab without one stamps "" and the drop takes the guarded legacy
      // branch rather than trusting a synthesized identity (#1779).
      const id = this.currentTabRealIds[index] ?? "";
      e.dataTransfer.setData(TAB_DND_MIME, JSON.stringify({ id, index, tabs: this.currentTabIds }));
      e.dataTransfer.effectAllowed = "move";
      document.body.classList.add("af-dragging-tab");
      // Remembered for the bar's own dragover, which cannot read the payload it just
      // stamped (#1813) — see dragFromIndex.
      this.dragFromIndex = index;
    });
    bar.addEventListener("dragend", () => {
      document.body.classList.remove("af-dragging-tab");
      this.dragFromIndex = null;
      this.hideTabInsert();
    });
  }

  /** Owns inline rename on the stable bar rather than on an individual button.
   *  Activating an inactive tab rebuilds the buttons synchronously on the first
   *  click of a double-click. Depending on geometry, Chromium may then target the
   *  second click at the replacement button or at the gap it moved away from, and
   *  may emit no useful dblclick at all. Capture the first click's stable identity,
   *  then consume click #2 before its button handler can rebuild again. This makes
   *  the intended tab — not a transient DOM node or coordinate — own the gesture. */
  private attachTabRename(bar: HTMLElement): void {
    let firstIdentity: string | null = null;
    bar.addEventListener(
      "click",
      (e) => {
        const target = e.target instanceof Element ? e.target.closest<HTMLElement>(".af-tab") : null;
        const button = target && bar.contains(target) ? target : null;
        const targetIntent = button ? tabRenameIntents.get(button) : undefined;

        if (e.detail === 1) {
          firstIdentity = targetIntent?.identity() ?? null;
          return;
        }
        if (e.detail !== 2) {
          firstIdentity = null;
          return;
        }

        const intendedIdentity = firstIdentity;
        firstIdentity = null;
        if (!intendedIdentity) {
          return;
        }

        // `detail === 2` is the browser's declaration that this is one multi-click
        // gesture. Its DOM target is not authoritative: rebuilding an overflowed bar
        // can reset scroll between the two clicks, putting an entirely different tab
        // under the unchanged pointer. Resolve the CURRENT button from the FIRST
        // click's identity, so the visual jump cannot turn rename into a second tab
        // selection or rename the neighbour that happened to move underneath it.
        const current = barTabs(bar).find(
          (candidate) => tabRenameIntents.get(candidate)?.identity() === intendedIdentity,
        );
        const intent = current ? tabRenameIntents.get(current) : undefined;
        if (!intent) {
          return;
        }

        // The first click already selected the tab. Stop the second click before its
        // ordinary openTab handler can replace the button whose edit is opening.
        e.preventDefault();
        e.stopPropagation();
        intent.begin();
      },
      { capture: true },
    );
  }

  /**
   * Wires the tab bar as a drop TARGET, for reordering tabs within it (#1813).
   *
   * The gesture is disambiguated from drag-to-split by WHERE the drag is released,
   * and the two regions are disjoint DOM subtrees that never overlap: the bar sits
   * inside the header, which is a sibling of the terminal host, and every split drop
   * handler is bound inside a .af-pane within that host (split.ts
   * wireDrop). A drag released over the strip reorders; over a pane it splits or
   * replaces. Neither handler can see the other's event — there is no shared
   * ancestor between them below <main>, so no bubbling to suppress and no
   * coordinate test to get wrong. That is also why the agent tab stays draggable:
   * it can't be reordered, but dragging it into a pane to split is the oldest
   * gesture the feature has, and pinning it at the DROP rather than the source
   * keeps that working.
   */
  private attachTabReorder(bar: HTMLElement): void {
    const isTabDrag = (e: DragEvent): boolean => e.dataTransfer?.types.includes(TAB_DND_MIME) ?? false;
    bar.addEventListener("dragover", (e: DragEvent) => {
      // The pinned agent tab is refused HERE, by declining to be a drop target at
      // all: no preventDefault means no drop event fires, so the browser shows a
      // "no drop" cursor and the gesture reads as impossible rather than as a
      // silently-ignored one. Dragging it onto a PANE is unaffected.
      if (!isTabDrag(e) || this.dragFromIndex === 0) {
        return;
      }
      e.preventDefault(); // allow the drop
      if (e.dataTransfer) {
        e.dataTransfer.dropEffect = "move";
      }
      this.showTabInsert(bar, e.clientX);
    });
    bar.addEventListener("dragleave", (e: DragEvent) => {
      // Ignore leave events that only cross into a child of the bar (a tab button).
      const to = e.relatedTarget as Node | null;
      if (to && bar.contains(to)) {
        return;
      }
      this.hideTabInsert();
    });
    bar.addEventListener("drop", (e: DragEvent) => {
      if (!isTabDrag(e)) {
        return;
      }
      e.preventDefault();
      this.hideTabInsert();
      const from = this.resolveBarDrag(e.dataTransfer?.getData(TAB_DND_MIME));
      if (from === null) {
        return;
      }
      const to = reorderTargetIndex(from, insertionIndexAt(tabCenters(bar), e.clientX));
      if (to === null) {
        return; // the agent tab, or a drop where the tab already sits
      }
      this.actions.reorderTab(from, to);
    });
  }

  /** The dragged tab's CURRENT ordinal, or null to ignore the drop. Resolves the
   *  payload by stable id exactly as a pane drop does (layout.ts resolveDragTab), so
   *  a tab closed or reordered by another client mid-drag cancels rather than moving
   *  whatever now sits at the drag-time index. */
  private resolveBarDrag(raw: string | undefined): number | null {
    if (!raw) {
      return null;
    }
    let drag: { id?: string; index: number; tabs: string[] };
    try {
      drag = JSON.parse(raw) as { id?: string; index: number; tabs: string[] };
    } catch {
      return null;
    }
    if (typeof drag.index !== "number" || !Array.isArray(drag.tabs)) {
      return null;
    }
    return resolveDragTab(drag, this.currentTabRealIds, this.currentTabIds, this.currentTabIds.length);
  }

  /** Draws the insertion indicator in the gap a drop at `clientX` would land in. */
  private showTabInsert(bar: HTMLElement, clientX: number): void {
    const marker = this.tabInsert;
    const tabs = barTabs(bar);
    if (!marker || tabs.length === 0) {
      return;
    }
    const at = insertionIndexAt(tabCenters(bar), clientX);
    // The gap's left edge: the next tab's left, or — for the gap past the end — the
    // last tab's right. Converted into the bar's own coordinates (its border box is
    // the marker's containing block) and offset by any horizontal scroll.
    const edge =
      at < tabs.length
        ? (tabs[at]?.getBoundingClientRect().left ?? 0)
        : (tabs[tabs.length - 1]?.getBoundingClientRect().right ?? 0);
    marker.style.left = `${edge - bar.getBoundingClientRect().left + bar.scrollLeft}px`;
    marker.hidden = false;
  }

  private hideTabInsert(): void {
    if (this.tabInsert) {
      this.tabInsert.hidden = true;
    }
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

    // Flip the selected rail row's lifecycle glyph + accessible verb when the
    // daemon-owned action changes without a selection change (#1932, #2186, #2234).
    // A fresh Snapshot usually rebuilds the rail too, but this patch keeps the
    // same-selection path correct without relying on that incidental repaint.
    const nowAction = selected.lifecycle_action ?? null;
    if (this.lifecycleBtn && nowAction && nowAction !== this.lifecycleAction) {
      this.patchLifecycleButton(this.lifecycleBtn, nowAction, selected.title);
      this.lifecycleAction = nowAction;
    }

    // Show/hide Retry as the selected session enters or leaves the usage-limit wall
    // (#1934). This is the load-bearing half of the button: a session almost always
    // hits the limit while it is the one you are watching, and that is not a
    // selection change, so renderMain never runs. Deciding visibility only at build
    // time would leave a limit-blocked session with no way out until the user
    // clicked away and back.
    const nowLimited = isLimitReached(selected);
    if (this.retryBtn && nowLimited !== this.retryVisible) {
      this.retryVisible = nowLimited;
      this.retryBtn.hidden = !nowLimited;
    }
  }
}

/** The product name, and the browser-tab title when there is nothing to qualify it. */
const APP_NAME = "Agent Factory";

/**
 * The browser-tab title for a state: "‹session› — ‹project› · Agent Factory" while a
 * session is selected, degrading to "‹project› · Agent Factory" when only a project is
 * scoped and to the bare app name when neither is. It reads the SELECTED session's own
 * repo root rather than the scoped project, so the title always names the project the
 * session actually lives in.
 *
 * Pure (state in, string out) and exported so it is unit-testable without a DOM;
 * AppShell.syncDocumentTitle owns the assignment.
 */
export function documentTitle(state: AppState): string {
  const sel = selectedSession(state);
  const root = sel?.worktree?.repo_path ?? state.selectedProject;
  const parts: string[] = [];
  if (sel && sel.title !== "") {
    parts.push(sel.title);
  }
  if (root) {
    parts.push(projectName(root));
  }
  const lead = parts.join(" — ");
  return lead === "" ? APP_NAME : `${lead} · ${APP_NAME}`;
}

/** The currently selected session row, or null. */
function selectedSession(state: AppState): SessionData | null {
  return state.selectedId ? (state.sessions.find((s) => s.id === state.selectedId) ?? null) : null;
}

/** A signature of everything the tab BAR draws for the selected session: which session
 *  it is, the ordered tabs (kind + name), the active index, the shown-in-a-pane set, and
 *  whether tabs are manageable (the create explanation / × affordances). The bar is rebuilt ONLY when
 *  this changes (ui update()), so an unrelated session-status snapshot — a rail event
 *  that leaves the tab list, highlight, and split layout alone — no longer
 *  replaceChildren()es the bar. That churn was the real cause of "a freshly-created tab
 *  can't be dragged": a new terminal tab's shell flaps status right after creation, and
 *  each snapshot destroyed the button the user had just grabbed, aborting the native
 *  HTML5 drag mid-gesture (#1737 follow-up).
 *
 *  Encoded with JSON.stringify over a STRUCTURED tuple, not a delimiter-joined string:
 *  a tab name containing a separator character must not be able to collide two distinct
 *  tab sets into the same signature and suppress a required rebuild. Exported for unit
 *  coverage.
 *
 *  THE PREMISE, stated because it is not this file's to enforce: the ordered [kind, name]
 *  list is a faithful key only because the daemon refuses to mint two tabs with the same
 *  name in one session (session/tab_names.go — uniqueShellName, "shell" → "shell-2";
 *  uniqueTabNameExcluding, "dup" → "dup-2"). That is what lets tab IDs stay OUT of this
 *  signature, which they must: adding them would rebuild the bar on an id backfill and
 *  reintroduce the #1737 drag-destroying churn above (the drag caches are synced outside
 *  this gate instead — see syncTabIdentityCaches). It is also what keeps every CAPTURED
 *  ordinal in the bar honest — tabButton's closeTab(index)/openTab(index), attachTabDrag's
 *  dragFromIndex, liveTabIdentity(index) — since a unique name means any permutation
 *  necessarily moves a [kind, name] and rebuilds the button. Were duplicate names ever to
 *  become representable (a daemon rule change, or tabLabel teaching a new kind to render
 *  `name`), a swap of two such tabs would be invisible here: no rebuild, and every one of
 *  those ordinals silently acts on the WRONG tab, with no error and no failing test.
 *  Whoever changes that rule owes this signature the id back, and #1737 a new answer. */
export function tabBarSig(state: AppState): string {
  const selected = selectedSession(state);
  if (!selected) {
    return "";
  }
  const tabs = sessionTabs(selected);
  const active = Math.min(Math.max(state.activeTab, 0), tabs.length - 1);
  const canManage = canManageTabs(selected);
  const createReason = tabCreationUnavailableReason(selected, tabs.length);
  const shown = [...new Set(state.shownTabs)].sort((a, b) => a - b);
  return JSON.stringify([selected.id ?? "", tabs.map((t) => [t.kind, t.name]), active, shown, canManage, createReason]);
}

/** The bar's tab buttons in render order. Excludes the + button and the insertion
 *  indicator, which are children of the bar but not tabs. */
function barTabs(bar: HTMLElement): HTMLElement[] {
  return [...bar.querySelectorAll<HTMLElement>(".af-tab")];
}

/** The rename intent captured by each currently rendered tab button. The stable bar
 *  carries a double-click across a button replacement by matching these identities;
 *  WeakMap ownership means detached buttons and their stale captures disappear
 *  together. */
const tabRenameIntents = new WeakMap<HTMLElement, { identity: () => string; begin: () => void }>();

/** Each tab's horizontal centre, in viewport px — the geometry the pure insertion
 *  math takes (tabreorder.ts). Measured rather than derived: tab widths vary with
 *  their labels, and a bar can scroll. */
function tabCenters(bar: HTMLElement): number[] {
  return barTabs(bar).map((t) => {
    const r = t.getBoundingClientRect();
    return r.left + r.width / 2;
  });
}

/** One tab-bar button: its label, an active-state highlight, a "shown in a pane"
 *  marker, and — for a closable (non-agent) tab of a tab-managed session — a × that
 *  closes it. Clicking the button points the focused pane at the tab AND attaches
 *  (like a session-row click); clicking the × closes the tab without attaching
 *  (stopPropagation so it isn't also a switch).
 *
 *  The button is also a DRAG SOURCE (feat: drag-and-drop split tabs): dragging it
 *  onto a pane edge splits that pane with this tab; onto a pane center replaces it.
 *  It carries draggable=true plus its index in data-tab-index; the actual dragstart is
 *  handled once, via delegation, on the bar container (AppShell.attachTabDrag) so a tab
 *  created after load is a drag source with no per-button rebinding (#1737). */
function tabButton(
  tab: { id?: string; name: string; kind: number },
  index: number,
  active: boolean,
  shown: boolean,
  canManage: boolean,
  actions: Actions,
  /** This tab's identity as of the LATEST snapshot — see AppShell.liveTabIdentity.
   *  A getter rather than a value because this button outlives the render that built
   *  it: it is called when a GESTURE fires, so the identity is the one the roster the
   *  user is looking at carries, not the one `tab` happened to be stamped with. */
  liveIdentity: () => string,
  /** The selected session's id as of THIS render — a value, not a getter, precisely
   *  because it must be the session the edit OPENS on: the bar is rebuilt whenever the
   *  selection changes (a new tab set, a new signature), so a button only ever belongs
   *  to the session selected when it was built. Carried into an inline rename so its
   *  commit can tell a vanished tab from the user's own session switch. */
  selectedSessionId: string,
): HTMLElement {
  const cls = `af-tab${active ? " af-tab-active" : ""}${shown && !active ? " af-tab-shown" : ""}`;
  const btn = h("button", { type: "button", class: cls, draggable: true });
  btn.setAttribute("role", "tab");
  btn.setAttribute("aria-selected", active ? "true" : "false");
  // The index the delegated dragstart reads to build the drag payload.
  btn.dataset.tabIndex = String(index);
  // The kind glyph (#1813), a DECORATIVE sibling of the label rather than part of
  // its text: the pane headers render the very same glyph+label pair from the very
  // same two functions, so the two surfaces agree by construction — but keeping the
  // glyph out of .af-tab-label leaves that node's text the bare label, which is what
  // both the bar's own aria-selected semantics and every existing label assertion
  // read. tabDisplayLabel() is the same pair as one string, used for the title.
  const glyph = h("span", { class: "af-tab-glyph" }, tabGlyph(tab.kind));
  glyph.setAttribute("aria-hidden", "true");
  btn.append(glyph, h("span", { class: "af-tab-label" }, tabLabel(tab)));
  btn.addEventListener("click", () => actions.openTab(index));
  // Rename-in-place (#1813), offered ONLY where a name is actually rendered: an
  // agent/shell tab draws a fixed label and ignores its name, so an edit there could
  // only appear to work (see isRenameableTab). A tab-managed session is required for
  // the same reason the + / × are: an archived/remote session's tab list is not the
  // web's to mutate.
  const renameable = canManage && isRenameableTab(tab.kind);
  btn.title = renameable ? `${tabDisplayLabel(tab)} — double-click to rename` : tabDisplayLabel(tab);
  if (renameable) {
    tabRenameIntents.set(btn, {
      identity: liveIdentity,
      begin: () => {
        // Resolved HERE, as the edit OPENS — the identity is captured, never a getter
        // the commit could re-read against a roster the user never saw. See
        // beginTabRename. The session id is captured with it, for the same reason.
        beginTabRename(btn, tab, actions, liveIdentity(), selectedSessionId);
      },
    });
  }
  // The agent tab (index 0) is unclosable — killing the session tears it down.
  if (index > 0 && canManage) {
    const close = h("span", { class: "af-tab-close", title: "Close tab" }, "×");
    close.setAttribute("aria-hidden", "true");
    close.addEventListener("click", (e) => {
      e.stopPropagation();
      actions.closeTab(index);
    });
    btn.append(close);
  }
  return btn;
}

/**
 * Swaps a tab button for an inline edit of its NAME (#1813): Enter or blur commits,
 * Escape cancels, and either way the button goes back in its place.
 *
 * The input REPLACES the button rather than nesting inside it — an <input> within a
 * <button> is invalid HTML and swallows its own clicks and keys.
 *
 * What is edited is `tab.name`, never the rendered label: they differ for a web tab
 * with no name (which draws "Web"), and seeding the field with "Web" would turn a
 * blur into a rename to the literal string "Web". Committing is likewise gated on a
 * real change to a non-empty name, so the two ways to leave an edit untouched —
 * blurring immediately, or clearing the field — both do nothing rather than fire a
 * request the daemon would reject.
 *
 * The daemon, not this input, decides the final name (it may dup-suffix, `dup` →
 * `dup-2`), and the session.updated it publishes is what repaints the bar. So the
 * button restored here still carries the OLD name; it is a placeholder until that
 * event lands, never a claim the rename resolved to what was typed.
 *
 * What the commit CARRIES is the tab's stable identity — never the ordinal it was
 * drawn at. The two are not interchangeable: an ordinal names a tab only relative to
 * one roster, and an edit spans two whenever another window reorders or closes a lower
 * tab while the input is open. That is not a hypothetical race but the ordinary path,
 * because the repaint which serves that change is itself what evicts the input from the
 * bar, and evicting a focused input FIRES ITS BLUR — so the stale slot would be
 * dereferenced by the very event that ends the edit, and whichever tab had shifted into
 * it would be renamed instead. renderTabBar settles an open edit before it rebuilds;
 * this makes that commit name the right tab.
 *
 * That identity is CAPTURED WHEN THE EDIT BEGINS (`editedId`) and carried to the
 * commit — never re-read at commit. An edit has DURATION, and the two readings answer
 * different questions: "which tab is the user editing?" versus "who is sitting at this
 * position NOW?". Only the first is the rename's subject.
 *
 * They come apart on a path the signature cannot see. tabBarSig covers what the bar
 * DRAWS — kind/name/active/shown — so another client CLOSING a tab and RECREATING one
 * with the same name in the same slot is signature-identical: the bar is not rebuilt,
 * this button and its open input survive, and the per-snapshot cache a commit-time read
 * consults has meanwhile been restamped with the REPLACEMENT's id. That is not a narrow
 * race: events dropped while the socket is down are never replayed, so the reconnect's
 * single re-Snapshot (events.ts) is exactly where a close and a recreate arrive fused
 * into one roster change. Committing that read renames a tab the user never edited.
 *
 * A captured id cannot go stale in the opposite direction, and that is structural
 * rather than lucky: every tab the daemon serves carries an id (every Tab constructor
 * mints one; restoreLocalTabs backfills a legacy pre-#1738 record on load), ids are
 * minted once and never reused, and tabIdentity therefore never falls back to its
 * synthesized kind:name here — the selftest asserts that premise against a live
 * Snapshot, because it is enforced two layers away in another language. So the only
 * thing that can change the identity at an index is a DIFFERENT tab arriving, which
 * must abort rather than inherit the edit.
 *
 * When the captured id no longer resolves, the tab being edited is GONE: index.ts
 * reports a miss and renames nothing. It deliberately does NOT fall back to the ordinal
 * or to whoever now holds the slot — that is this same bug wearing a helpful face.
 */
function beginTabRename(
  btn: HTMLElement,
  tab: { id?: string; name: string; kind: number },
  actions: Actions,
  /** The edited tab's identity, resolved as the edit OPENS (tabButton). A value, not
   *  a getter: the capture is what makes the commit name the right tab, so the type
   *  is what keeps a later reader from re-reading it. */
  editedId: string,
  /** The session the edit opened on, captured with editedId (tabButton). Carried to
   *  the commit so it can tell a vanished tab (report a miss) from the user switching
   *  session while the input was open (abandon silently). */
  editedSessionId: string,
): void {
  const input = h("input", { type: "text", class: "af-tab-edit", value: tab.name });
  input.setAttribute("aria-label", `Rename tab ${tabLabel(tab)}`);
  let done = false;
  const finish = (commit: boolean): void => {
    if (done) {
      return; // Enter/Escape already settled it; the blur they cause is a no-op
    }
    done = true;
    const next = input.value.trim();
    input.replaceWith(btn);
    if (commit && next !== "" && next !== tab.name) {
      actions.renameTab(editedId, next, editedSessionId);
    }
  };
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      finish(true);
    } else if (e.key === "Escape") {
      // Settled synchronously, BEFORE the blur this may cause: `done` then makes
      // that blur's commit a no-op, so Escape can never save what it cancelled.
      e.preventDefault();
      finish(false);
    }
  });
  input.addEventListener("blur", () => finish(true));
  // A drag started on the input would be a text selection, not a tab drag; the bar's
  // delegated dragstart only fires for a .af-tab, so there is nothing to suppress.
  btn.replaceWith(input);
  input.focus();
  input.select();
}

/** One session row: a status dot, the (prefixed) title, the branch line, and a reserved
 *  slot for its quiet lifecycle actions (#2186, #2223). Clicking opens the session by
 *  its stable id (selects it and attaches its terminal, #1693); a row lacking an id
 *  (never expected from a live Snapshot) is rendered but inert. */
function sessionRow(
  s: SessionData,
  selected: boolean,
  openSession: (id: string) => void,
  buildActions: (session: ActionableSession) => HTMLElement,
): HTMLElement {
  const status = rowStatus(s);
  const creating = isCreating(s);
  const actionable = isActionableSession(s);

  const title = h("div", { class: "af-row-title" }, rowTitle(s));
  const branch = h(
    "div",
    { class: "af-row-branch" },
    h("span", { class: "af-branch-icon" }, "⎇"),
    " ",
    s.branch || "—",
  );
  const main = h("div", { class: "af-row-main" }, title, branch);

  const cls = `af-row${selected ? " af-row-selected" : ""}${isArchived(s) ? " af-row-archived" : ""}${
    actionable ? "" : " af-row-inert"
  }${creating ? " af-row-creating" : ""}`;
  const row = h("li", { class: cls });
  // A working/busy row shows NO status dot (#1766) — only Ready/error states draw
  // one. When there is no dot the span is omitted entirely (kind is null), matching
  // the TUI's blank status cell.
  if (status.kind) {
    const dot = h("span", { class: `af-dot af-dot-${status.kind}` }, status.glyph);
    dot.setAttribute("aria-hidden", "true");
    row.append(dot);
  }
  row.append(main);
  if (actionable) {
    row.append(buildActions(s));
  }
  row.setAttribute("role", "option");
  row.setAttribute("aria-selected", selected ? "true" : "false");
  row.setAttribute("title", `${s.title} — ${status.label}`);
  if (!actionable) {
    // The server withheld a lifecycle action: a creating row has no session yet,
    // while an id-less row has no unambiguous destructive target. Selection and
    // lifecycle controls consume the same fail-closed capability.
    row.setAttribute("aria-disabled", "true");
  } else {
    row.addEventListener("click", () => openSession(s.id));
  }
  return row;
}
