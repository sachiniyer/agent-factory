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
import { ProjectsPane } from "./projects.js";
import { compareSessionsForRail, isArchived, rowStatus, rowTitle } from "./status.js";
import { TasksPane } from "./tasks.js";
import type { TerminalStatus } from "./terminal.js";
import type { SessionData, TaskData } from "./types.js";

/** The whole client state: which view to show, the login details, and — once
 *  authed — the live session projection plus the current selection. */
export interface AppState {
  phase: "login" | "app";
  /** the top-level view (#1592 Phase 5 PR8): the live sessions rail+terminal, the
   *  projects (repo grouping) pane, or the tasks (scheduled automations) pane. The
   *  appbar view tabs and the [ / ] keys switch it; it selects which body shows. */
  view: View;
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
  /** Switches the top-level view (#1592 Phase 5 PR8): the appbar view tabs and the
   *  [ / ] keys route here; index.ts flips the store and hands the keyboard back to
   *  the rail (blurring the terminal) when leaving the sessions view. */
  switchView(view: View): void;
  /** Opens the add-task modal (#1592 Phase 5 PR8). */
  addTask(): void;
  /** Enables/disables a task via UpdateTask. */
  toggleTask(task: TaskData): void;
  /** Fires a task now via TriggerTask (enabled cron tasks). */
  triggerTask(task: TaskData): void;
  /** Removes a task via RemoveTask. */
  removeTask(task: TaskData): void;
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
export function sessionTabs(s: SessionData): { name: string; kind: number }[] {
  if (s.tabs && s.tabs.length > 0) {
    return s.tabs;
  }
  return [{ name: "agent", kind: 0 }];
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
  return tab.name || "Tab";
}

/** The distinct repo roots the current sessions belong to — the new-session
 *  modal's project picker, derived from the live projection exactly as the TUI's
 *  zero-config picker is (app/switch_project.go buildProjectListFrom). Sorted for a
 *  stable menu. */
export function deriveProjects(sessions: SessionData[]): string[] {
  const roots = new Set<string>();
  for (const s of sessions) {
    const root = s.worktree?.repo_path;
    if (root) {
      roots.add(root);
    }
  }
  return [...roots].sort();
}

/** The appbar label for a top-level view (#1592 Phase 5 PR8). */
function viewLabel(view: View): string {
  switch (view) {
    case "sessions":
      return "Sessions";
    case "projects":
      return "Projects";
    case "tasks":
      return "Tasks";
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
  private readonly sessionsBody: HTMLElement;
  private readonly projectsPane: ProjectsPane;
  private readonly tasksPane: TasksPane;
  private lastView: View | null = null;
  private lastTasks: TaskData[] | null = null;

  // Header text nodes for the selected pane, (re)created per selection.
  private headTitle: HTMLElement | null = null;
  private headMeta: HTMLElement | null = null;
  // The tab bar for the selected session, (re)created per selection and patched in
  // place when the tab list or active tab changes (#1592 Phase 5 PR7). null when
  // nothing is selected (the empty state has no tabs).
  private tabBar: HTMLElement | null = null;

  // Last-applied state, for cheap change detection between updates.
  private lastSessions: SessionData[] | null = null;
  private lastSelectedId: string | null = null;
  private lastLive: EventStreamStatus | null = null;
  private lastKb: KeyboardFocus | null = null;
  private lastActiveTab = 0;
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

    const header = h(
      "header",
      { class: "af-appbar" },
      h("span", { class: "af-brand" }, "Agent Factory"),
      viewNav,
      live,
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

    // The projects + tasks views are peers of the sessions body inside one viewport;
    // update() shows exactly one and hides the others by `state.view`. They own their
    // own subtrees so a task.* / rail event patches only the active pane.
    this.projectsPane = new ProjectsPane((id: string) => this.actions.open(id));
    this.tasksPane = new TasksPane({
      add: () => this.actions.addTask(),
      toggle: (task: TaskData) => this.actions.toggleTask(task),
      trigger: (task: TaskData) => this.actions.triggerTask(task),
      remove: (task: TaskData) => this.actions.removeTask(task),
    });
    const viewport = h("div", { class: "af-viewport" }, this.sessionsBody, this.projectsPane.el, this.tasksPane.el);

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

    // View switching (#1592 Phase 5 PR8): show the active view's body, hide the
    // others, and highlight its appbar tab. The sessions body is only HIDDEN (never
    // removed), so the terminal host inside it — and its focus/scrollback — survives
    // a round trip to another view.
    if (this.lastView !== state.view) {
      this.lastView = state.view;
      this.sessionsBody.hidden = state.view !== "sessions";
      this.projectsPane.el.hidden = state.view !== "projects";
      this.tasksPane.el.hidden = state.view !== "tasks";
      for (const [v, tab] of this.viewTabs) {
        tab.classList.toggle("af-viewtab-active", v === state.view);
        tab.setAttribute("aria-selected", v === state.view ? "true" : "false");
      }
    }

    // The projects pane mirrors the live session grouping; the tasks pane mirrors the
    // task projection. Each self-guards on an unchanged reference, so these are cheap
    // no-ops when their data didn't change (and when their view isn't showing).
    this.projectsPane.update(state.sessions, state.selectedId);
    if (this.lastTasks !== state.tasks) {
      this.lastTasks = state.tasks;
      this.tasksPane.update(state.tasks);
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

    // The main pane's STRUCTURE only changes when the selected session changes (or on
    // the very first update, which lays down the initial empty-state placeholder);
    // otherwise we just patch its header text (status/title/branch), leaving the
    // terminal host — and its focus and scrollback — in place.
    const activeTabChanged = this.lastActiveTab !== state.activeTab;
    this.lastActiveTab = state.activeTab;
    if (selectionChanged || !this.mainRendered) {
      this.mainRendered = true;
      this.renderMain(state);
    } else {
      this.patchMainHead(state);
      // The tab bar reflects the live tab list and the active-tab highlight; either
      // can change without a selection change (a resync grows/shrinks the list, or
      // a 1-9 key moves the highlight). Rebuilding just the bar leaves termHost —
      // and the focused xterm inside it — untouched.
      if (sessionsChanged || activeTabChanged) {
        this.renderTabBar(state);
      }
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

    const children: HTMLElement[] = tabs.map((tab, i) =>
      tabButton(tab, i, i === active, shown.has(i), canManage, this.actions),
    );
    if (canManage && tabs.length < MAX_TABS) {
      const add = h("button", { type: "button", class: "af-tab-new", title: "New tab" }, "+");
      add.addEventListener("click", () => this.actions.newTab());
      children.push(add);
    }
    bar.replaceChildren(...children);
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

/** One tab-bar button: its label, an active-state highlight, a "shown in a pane"
 *  marker, and — for a closable (non-agent) tab of a tab-managed session — a × that
 *  closes it. Clicking the button points the focused pane at the tab AND attaches
 *  (like a session-row click); clicking the × closes the tab without attaching
 *  (stopPropagation so it isn't also a switch).
 *
 *  The button is also a DRAG SOURCE (feat: drag-and-drop split tabs): dragging it
 *  onto a pane edge splits that pane with this tab; onto a pane center replaces it.
 *  The dragged tab index rides the dataTransfer under a private MIME the panes read. */
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
  btn.append(h("span", { class: "af-tab-label" }, tabLabel(tab)));
  btn.addEventListener("click", () => actions.openTab(index));
  // Drag source: stamp the tab index and flag the body so panes can show drop hints.
  btn.addEventListener("dragstart", (e) => {
    if (!e.dataTransfer) {
      return;
    }
    e.dataTransfer.setData(TAB_DND_MIME, String(index));
    e.dataTransfer.effectAllowed = "move";
    document.body.classList.add("af-dragging-tab");
  });
  btn.addEventListener("dragend", () => document.body.classList.remove("af-dragging-tab"));
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
    row.addEventListener("click", () => actions.open(id));
  }
  return row;
}
