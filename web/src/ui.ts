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
import { TAB_DND_MIME } from "./layout.js";
import { projectMeta, projectName, type ProjectSummary, projectSummaries, scopeToProject } from "./project.js";
import { compareSessionsForRail, isArchived, rowStatus, rowTitle } from "./status.js";
import { TasksPane } from "./tasks.js";
import { type ThemeChoice, THEME_CHOICES } from "./theme.js";
import type { TerminalStatus } from "./terminal.js";
import type { SessionData, TaskData } from "./types.js";

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
  /** the persisted theme preference (redesign PR1): Auto follows the OS, Light/Dark
   *  force a mode. The appbar toggle sets it; theme.ts stamps data-theme on <html>
   *  and re-themes the live terminals. */
  themeChoice: ThemeChoice;
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
  /** Opens the send-prompt modal for the current selection. */
  sendPrompt(): void;
  /** Opens the kill-confirm modal for the current selection. */
  kill(): void;
  /** Opens the archive-confirm modal for the current selection. */
  archive(): void;
  /** Switches the selected session's active tab WITHOUT attaching — the keyboard
   *  stays in rail nav mode (the 1-9 keys, mirroring the TUI). */
  switchTab(index: number): void;
  /** Switches to a tab AND attaches its terminal (a tab-bar click, mirroring how a
   *  session-row click attaches). */
  openTab(index: number): void;
  /** Creates a new $SHELL tab on the selected session (the `t` key / + button). */
  newTab(): void;
  /** Closes the tab at `index` of the selected session (the `w` key / × button);
   *  the agent tab (index 0) is unclosable. */
  closeTab(index: number): void;
  /** Switches the top-level view: the appbar view tabs and the [ / ] keys route
   *  here; index.ts flips the store and hands the keyboard back to the rail (blurring
   *  the terminal) when leaving the sessions view. */
  switchView(view: View): void;
  /** Switches the active project (redesign PR2): the top-right switcher menu routes
   *  here; index.ts scopes the rail + views to `root`, persists it, and drops a
   *  selection that no longer belongs to the new project. */
  switchProject(root: string): void;
  /** Opens the add-task modal (#1592 Phase 5 PR8). */
  addTask(): void;
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
 *  up to eight shell/process tabs, matching the 1-9 number-key range. The + button
 *  hides at the cap so the web never fires a guaranteed-to-fail CreateTab. */
const MAX_TABS = 9;

/** Whether a session supports user tab management: remote-hook sessions have their
 *  tabs fixed by config (daemon Capabilities().TabManagement), so the web hides
 *  their + / × affordances and gates the `t`/`w` keys. */
export function supportsTabManagement(s: SessionData): boolean {
  return s.backend_type !== "remote";
}

/** The selected session's tabs, always non-empty: a pre-#930 record with no tabs
 *  is shown as a single implicit agent tab so the bar (and index math) never sees
 *  an empty list. */
export function sessionTabs(s: SessionData): { name: string; kind: number; url?: string }[] {
  if (s.tabs && s.tabs.length > 0) {
    return s.tabs;
  }
  return [{ name: "agent", kind: 0 }];
}

/** A stable-ish client-side IDENTITY for a tab (feat: split tabs), used to detect a
 *  tab set that changed mid-drag. It is NOT a daemon tab id (there is none yet — the
 *  full stable-id fix is #1738); it is the best identity the client has: the tab's
 *  kind + name. The ordered list of these is snapshotted at dragstart and re-checked
 *  at drop, so a concurrent close/create/reorder cancels the drop instead of binding a
 *  pane to the wrong live tab. */
export function tabIdentity(tab: { name: string; kind: number }): string {
  return `${tab.kind}:${tab.name}`;
}

/** The label a tab reads as, mirroring the TUI's labelForTab (ui/tree/labels.go):
 *  the agent tab is "Agent", a shell tab is "Terminal", a process tab shows its
 *  name. Keeps the web tab bar TUI-faithful. */
function tabLabel(tab: { name: string; kind: number }): string {
  if (tab.kind === 0) {
    return "Agent";
  }
  if (tab.kind === 1) {
    return "Terminal";
  }
  if (tab.kind === 3) {
    return tab.name || "Web";
  }
  return tab.name || "Tab";
}

/** The appbar label for a top-level view. */
function viewLabel(view: View): string {
  switch (view) {
    case "sessions":
      return "Sessions";
    case "tasks":
      return "Tasks";
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

  // Header text nodes for the selected pane, (re)created per selection.
  private headTitle: HTMLElement | null = null;
  private headMeta: HTMLElement | null = null;
  // The tab bar for the selected session, (re)created per selection and patched in
  // place when the tab list or active tab changes (#1592 Phase 5 PR7). null when
  // nothing is selected (the empty state has no tabs).
  private tabBar: HTMLElement | null = null;
  // The tab identities (kind:name) drawn in the bar at its last render, stamped into a
  // dragged tab's payload by the delegated dragstart so a drop can detect a mid-drag
  // tab-set change and cancel (see split.ts). Kept live by renderTabBar.
  private currentTabIds: string[] = [];
  // A signature of everything the bar DRAWS (see tabBarSig): the bar is rebuilt only
  // when this changes, so an unrelated status snapshot never churns its DOM (#1737).
  private lastTabBarSig = "";

  // Last-applied state, for cheap change detection between updates.
  private lastSessions: SessionData[] | null = null;
  private lastSelectedId: string | null = null;
  private lastLive: EventStreamStatus | null = null;
  private lastKb: KeyboardFocus | null = null;
  private lastError: string | null = null;
  // Whether the main pane has been rendered at least once. The constructor leaves it
  // an empty <section>, so the FIRST update must render it even when nothing is
  // selected (selectedId is null before AND after that first update, so the
  // selection-changed guard alone wouldn't fire) — otherwise the pane is blank on
  // load until a select-then-deselect. (#1592 Phase 5 PR9)
  private mainRendered = false;

  constructor(
    private readonly actions: Actions,
    private readonly termHost: HTMLElement,
    private readonly modalHost: HTMLElement,
  ) {
    this.pip = h("span", { class: "af-live-pip" });
    this.pip.setAttribute("aria-hidden", "true");
    this.pipLabel = h("span", { class: "af-live-label" });
    const live = h("span", { class: "af-live" }, this.pip, this.pipLabel);
    live.setAttribute("role", "status");

    const disconnect = h("button", { type: "button", class: "af-ghost" }, "Disconnect");
    disconnect.addEventListener("click", () => this.actions.disconnect());

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

    const header = h(
      "header",
      { class: "af-appbar" },
      h("span", { class: "af-brand" }, "Agent Factory"),
      viewNav,
      this.projectSwitchWrap,
      live,
      themeToggle,
      disconnect,
    );

    this.railCount = h("span", { class: "af-rail-count" }, "0");
    const newBtn = h("button", { type: "button", class: "af-rail-new", title: "New session" }, "+ New");
    newBtn.addEventListener("click", () => this.actions.newSession());
    const railHead = h(
      "div",
      { class: "af-rail-head" },
      h("span", { class: "af-rail-title" }, "Sessions"),
      this.railCount,
      newBtn,
    );
    this.railList = h("ul", { class: "af-rail-list" });
    this.railList.setAttribute("role", "listbox");
    this.railList.setAttribute("aria-label", "Sessions");
    const rail = h("nav", { class: "af-rail" }, railHead, this.railList);

    this.main = h("section", { class: "af-main" });
    this.sessionsBody = h("div", { class: "af-body" }, rail, this.main);

    // The tasks view is a peer of the sessions body inside one viewport; update()
    // shows exactly one and hides the other by `state.view`. It owns its own subtree
    // (scoped to the selected project) so a task.* event patches only that pane.
    this.tasksPane = new TasksPane({
      add: () => this.actions.addTask(),
      toggle: (task: TaskData) => this.actions.toggleTask(task),
      trigger: (task: TaskData) => this.actions.triggerTask(task),
      remove: (task: TaskData) => this.actions.removeTask(task),
    });
    const viewport = h("div", { class: "af-viewport" }, this.sessionsBody, this.tasksPane.el);

    // A transient toast for failed tab ops: a fixed-position banner that fades in
    // only while `tabError` is set (index.ts clears it on a timer / selection change).
    this.toast = h("div", { class: "af-toast" });
    this.toast.setAttribute("role", "alert");
    // The modal host is a persistent overlay layer index.ts mounts modals into; it
    // sits above the app body and is empty except while a modal is open.
    this.el = h("main", { class: "af-app" }, header, viewport, this.toast, this.modalHost);
  }

  /** Applies the latest state, touching only what changed. */
  update(state: AppState): void {
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
      for (const [v, tab] of this.viewTabs) {
        tab.classList.toggle("af-viewtab-active", v === state.view);
        tab.setAttribute("aria-selected", v === state.view ? "true" : "false");
      }
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

    const sessionsChanged = this.lastSessions !== state.sessions;
    const selectionChanged = this.lastSelectedId !== state.selectedId;
    const projectChanged = this.lastSelectedProject !== state.selectedProject;

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

    // Rebuild the rail when the list, the highlighted row, OR the project scope
    // changed (switching projects swaps the rail). None touches the terminal host (it
    // lives in the main pane), so events never blur it.
    if (sessionsChanged || selectionChanged || projectChanged) {
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
  }

  /** Renders the rail SCOPED to the selected project (redesign PR2): only that
   *  project's sessions, never the whole projection. Three states: no project at all
   *  (nothing created yet) → the "no sessions yet" empty rail; a project with no LIVE
   *  sessions → the dim one-line per-project empty state; otherwise the scoped rows. */
  private renderRail(state: AppState): void {
    const scoped = scopeToProject(state.sessions, state.selectedProject);
    this.railCount.textContent = String(scoped.length);
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
    const rows = orderedSessions(scoped).map((s) => sessionRow(s, s.id === state.selectedId, this.actions));
    // A selected project with no LIVE sessions (all archived / none): a clean, dim
    // one-liner with a + New affordance rather than a blank rail (redesign PR2). Any
    // archived rows still render below it so the history stays reachable.
    const hasLive = scoped.some((s) => !isArchived(s));
    if (!hasLive) {
      const name = projectName(state.selectedProject);
      const newBtn = h("button", { type: "button", class: "af-rail-empty-new", title: "New session" }, "+ New");
      newBtn.addEventListener("click", () => this.actions.newSession());
      const empty = h("li", { class: "af-rail-empty-project" }, `No sessions in ${name} — `, newBtn);
      list.replaceChildren(empty, ...rows);
      return;
    }
    list.replaceChildren(...rows);
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

    // Per-session actions (#1592 Phase 5 PR5): send a prompt, or kill/archive
    // behind a confirm. They act on the current selection; index.ts reads it.
    const promptBtn = h("button", { type: "button", class: "af-ghost af-term-action" }, "Prompt");
    promptBtn.addEventListener("click", () => this.actions.sendPrompt());
    const archiveBtn = h("button", { type: "button", class: "af-ghost af-term-action" }, "Archive");
    archiveBtn.addEventListener("click", () => this.actions.archive());
    const killBtn = h("button", { type: "button", class: "af-danger af-term-action" }, "Kill");
    killBtn.addEventListener("click", () => this.actions.kill());
    const actions = h("div", { class: "af-term-actions" }, promptBtn, archiveBtn, killBtn);

    const head = h("div", { class: "af-term-head" }, titleBox, actions);

    // The tab bar sits between the header and the terminal, mirroring the TUI's
    // tab row (ui/tabbed_window.go): one button per tab, the active one highlighted,
    // a × on closable (non-agent) tabs, and a + to add a shell tab.
    this.tabBar = h("div", { class: "af-tabbar" });
    this.tabBar.setAttribute("role", "tablist");
    this.tabBar.setAttribute("aria-label", "Session tabs");
    // The drag source is wired ONCE here on the (stable) bar container via delegation,
    // not per button — so EVERY tab, including one created after load, is a drag source
    // by construction, with no per-button binding to forget on a re-render (#1737).
    this.attachTabDrag(this.tabBar);

    this.main.className = "af-main af-main-term";
    // The persistent terminal host is (re)mounted here; renderMain runs only on a
    // selection change, so this reparent is rare and never happens mid-type.
    this.main.replaceChildren(head, this.tabBar, this.termHost);
    this.renderTabBar(state);
    this.patchMainHead(state);
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
    const tabs = sessionTabs(selected);
    const canManage = supportsTabManagement(selected);
    // The active index is clamped: a resync that shrank the list must not leave the
    // highlight (and the streamed tab) pointing past the end.
    const active = Math.min(Math.max(state.activeTab, 0), tabs.length - 1);
    // Which tabs are currently rendered in a pane (feat: split tabs), so the bar can
    // mark an already-open tab distinctly from the focused one.
    const shown = new Set(state.shownTabs);
    // The ordered tab identities at THIS render, kept for the delegated dragstart to
    // stamp into a dragged tab's payload so the drop can detect a mid-drag tab-set
    // change and cancel (split.ts).
    this.currentTabIds = tabs.map(tabIdentity);

    const children: HTMLElement[] = tabs.map((tab, i) =>
      tabButton(tab, i, i === active, shown.has(i), canManage, this.actions),
    );
    if (canManage && tabs.length < MAX_TABS) {
      const add = h("button", { type: "button", class: "af-tab-new", title: "New tab" }, "+");
      add.addEventListener("click", () => this.actions.newTab());
      children.push(add);
    }
    bar.replaceChildren(...children);
    // Rebuilding the bar detaches EVERY tab button — including the source of an
    // in-flight drag. A native dragend can't fire on a detached node, so the delegated
    // dragend (on the bar) would never run and the drag flag would stick, leaving the UI
    // in "dragging" mode (pane hints + drop overlay, both gated on the flag). Clear it
    // here so a drag that loses its source still ends cleanly. A no-op when idle.
    document.body.classList.remove("af-dragging-tab");
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
      e.dataTransfer.setData(TAB_DND_MIME, JSON.stringify({ index, tabs: this.currentTabIds }));
      e.dataTransfer.effectAllowed = "move";
      document.body.classList.add("af-dragging-tab");
    });
    bar.addEventListener("dragend", () => document.body.classList.remove("af-dragging-tab"));
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

/** A signature of everything the tab BAR draws for the selected session: which session
 *  it is, the ordered tabs (kind + name), the active index, the shown-in-a-pane set, and
 *  whether tabs are manageable (the + / × affordances). The bar is rebuilt ONLY when
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
 *  coverage. */
export function tabBarSig(state: AppState): string {
  const selected = selectedSession(state);
  if (!selected) {
    return "";
  }
  const tabs = sessionTabs(selected);
  const active = Math.min(Math.max(state.activeTab, 0), tabs.length - 1);
  const canManage = supportsTabManagement(selected);
  const shown = [...new Set(state.shownTabs)].sort((a, b) => a - b);
  return JSON.stringify([selected.id ?? "", tabs.map((t) => [t.kind, t.name]), active, shown, canManage]);
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
  tab: { name: string; kind: number },
  index: number,
  active: boolean,
  shown: boolean,
  canManage: boolean,
  actions: Actions,
): HTMLElement {
  const cls = `af-tab${active ? " af-tab-active" : ""}${shown && !active ? " af-tab-shown" : ""}`;
  const btn = h("button", { type: "button", class: cls, draggable: true });
  btn.setAttribute("role", "tab");
  btn.setAttribute("aria-selected", active ? "true" : "false");
  // The index the delegated dragstart reads to build the drag payload.
  btn.dataset.tabIndex = String(index);
  btn.append(h("span", { class: "af-tab-label" }, tabLabel(tab)));
  btn.addEventListener("click", () => actions.openTab(index));
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

/** One session row: a status dot, the (prefixed) title, and the branch line —
 *  the same three signals the TUI row carries (ui/tree/render.go). Clicking opens
 *  the session by its stable id (selects it and attaches its terminal, #1693); a
 *  row lacking an id (never expected from a live Snapshot) is rendered but inert. */
function sessionRow(s: SessionData, selected: boolean, actions: Actions): HTMLElement {
  const status = rowStatus(s);

  const title = h("div", { class: "af-row-title" }, rowTitle(s));
  const branch = h(
    "div",
    { class: "af-row-branch" },
    h("span", { class: "af-branch-icon" }, "⎇"),
    " ",
    s.branch || "—",
  );
  const main = h("div", { class: "af-row-main" }, title, branch);

  const cls = `af-row${selected ? " af-row-selected" : ""}${isArchived(s) ? " af-row-archived" : ""}`;
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
  row.setAttribute("role", "option");
  row.setAttribute("aria-selected", selected ? "true" : "false");
  row.setAttribute("title", `${s.title} — ${status.label}`);
  if (s.id) {
    const id = s.id;
    row.addEventListener("click", () => actions.open(id));
  }
  return row;
}
