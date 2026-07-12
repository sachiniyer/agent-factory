// src/api.ts
var TOKEN_KEY = "af.token";
function loadToken() {
  return sessionStorage.getItem(TOKEN_KEY);
}
function storeToken(token2) {
  sessionStorage.setItem(TOKEN_KEY, token2);
}
function clearToken() {
  sessionStorage.removeItem(TOKEN_KEY);
}
var ApiError = class extends Error {
  status;
  constructor(status, message) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
};
async function af(method, body, token2) {
  let resp;
  try {
    resp = await fetch(`/v1/${method}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token2}`
      },
      body: JSON.stringify(body ?? {})
    });
  } catch (e) {
    throw new ApiError(0, `cannot reach the daemon: ${e.message}`);
  }
  let env = null;
  try {
    env = await resp.json();
  } catch {
  }
  if (!resp.ok) {
    throw new ApiError(resp.status, env?.error ?? `${resp.status} ${resp.statusText}`);
  }
  if (env && env.error !== null) {
    throw new ApiError(resp.status, env.error);
  }
  return env?.data;
}
async function fetchSnapshot(token2) {
  const resp = await af("Snapshot", { repo_id: "" }, token2);
  return resp.instances ?? [];
}
function probeToken(token2) {
  return fetchSnapshot(token2);
}

// src/events.ts
var BACKOFF_BASE_MS = 500;
var BACKOFF_MAX_MS = 1e4;
function wsScheme() {
  return window.location.protocol === "https:" ? "wss:" : "ws:";
}
var EventStream = class {
  constructor(token2, cb) {
    this.token = token2;
    this.cb = cb;
  }
  ws = null;
  stopped = false;
  everOpened = false;
  retry = 0;
  reconnectTimer = null;
  /** Opens the socket and begins delivering events. Idempotent-ish: call once. */
  start() {
    this.stopped = false;
    this.open();
  }
  /** Permanently closes the stream and cancels any pending reconnect. */
  stop() {
    this.stopped = true;
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      this.ws.onopen = null;
      this.ws.onmessage = null;
      this.ws.onclose = null;
      this.ws.onerror = null;
      this.ws.close();
      this.ws = null;
    }
  }
  open() {
    this.cb.onStatus(this.everOpened ? "reconnecting" : "connecting");
    const url = `${wsScheme()}//${window.location.host}/v1/events?access_token=${encodeURIComponent(this.token)}`;
    let ws;
    try {
      ws = new WebSocket(url);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.ws = ws;
    ws.onopen = () => {
      this.retry = 0;
      const wasReconnect = this.everOpened;
      this.everOpened = true;
      this.cb.onStatus("open");
      if (wasReconnect) {
        this.cb.onResync();
      }
    };
    ws.onmessage = (e) => {
      if (typeof e.data !== "string") {
        return;
      }
      let ev;
      try {
        ev = JSON.parse(e.data);
      } catch {
        return;
      }
      if (ev && typeof ev.type === "string") {
        this.cb.onEvent(ev);
      }
    };
    ws.onclose = () => this.scheduleReconnect();
    ws.onerror = () => {
      try {
        ws.close();
      } catch {
      }
    };
  }
  scheduleReconnect() {
    if (this.stopped || this.reconnectTimer !== null) {
      return;
    }
    this.ws = null;
    this.cb.onStatus("reconnecting");
    const delay = Math.min(BACKOFF_BASE_MS * 2 ** this.retry, BACKOFF_MAX_MS);
    this.retry += 1;
    this.reconnectTimer = window.setTimeout(() => {
      this.reconnectTimer = null;
      if (!this.stopped) {
        this.open();
      }
    }, delay);
  }
};

// src/sessions.ts
function upsertSession(list, s) {
  const i = list.findIndex((x) => x.title === s.title);
  if (i === -1) {
    return [...list, s];
  }
  const next = list.slice();
  next[i] = s;
  return next;
}
function removeSession(list, title) {
  return list.filter((x) => x.title !== title);
}
function applyEvent(list, ev) {
  switch (ev.type) {
    case "session.created":
    case "session.updated":
      if (ev.data && ev.data.title) {
        return { sessions: upsertSession(list, ev.data), needsResync: false };
      }
      return { sessions: list, needsResync: false };
    case "session.killed":
      if (ev.data && ev.data.title) {
        return { sessions: removeSession(list, ev.data.title), needsResync: false };
      }
      return { sessions: list, needsResync: false };
    case "session.archived":
    case "session.restored":
      return { sessions: list, needsResync: true };
    default:
      return { sessions: list, needsResync: false };
  }
}
function pickSelection(list, current) {
  if (current && list.some((s) => s.title === current)) {
    return current;
  }
  return null;
}

// src/store.ts
var Store = class {
  state;
  listeners = /* @__PURE__ */ new Set();
  constructor(initial) {
    this.state = initial;
  }
  /** The current immutable snapshot of state. */
  get() {
    return this.state;
  }
  /** Merges a partial update and notifies every subscriber with the new state. */
  set(patch) {
    this.state = { ...this.state, ...patch };
    for (const listener of this.listeners) {
      listener(this.state);
    }
  }
  /** Registers a listener and returns an unsubscribe function. */
  subscribe(listener) {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }
};

// src/types.ts
var Liveness = {
  Unset: 0,
  Running: 1,
  Ready: 2,
  Lost: 3,
  Dead: 4,
  Archived: 5,
  LimitReached: 6
};
var InFlightOp = {
  None: 0,
  Creating: 1,
  Killing: 2,
  Archiving: 3,
  Restoring: 4
};
var Status = {
  Running: 0,
  Ready: 1,
  Loading: 2,
  Deleting: 3,
  Dead: 4,
  Lost: 5,
  Archived: 6
};

// src/status.ts
var READY_GLYPH = "\u25CF";
var DEAD_GLYPH = "\u25CB";
var LOST_GLYPH = "\u25CC";
var ARCHIVED_GLYPH = "\u25A7";
var LIMIT_GLYPH = "\u25C6";
var WORKING_GLYPH = "\u25CF";
var WORKING = { glyph: WORKING_GLYPH, kind: "working", spinning: true, label: "Working" };
function rowStatus(s) {
  const op = s.in_flight_op ?? InFlightOp.None;
  if (op !== InFlightOp.None) {
    return WORKING;
  }
  return dotForLiveness(livenessOf(s));
}
function livenessOf(s) {
  const lv = s.liveness ?? Liveness.Unset;
  if (lv !== Liveness.Unset) {
    return lv;
  }
  switch (s.status) {
    case Status.Ready:
      return Liveness.Ready;
    case Status.Dead:
      return Liveness.Dead;
    case Status.Lost:
      return Liveness.Lost;
    case Status.Archived:
      return Liveness.Archived;
    // Running and the transient values (Loading/Deleting, which never persist)
    // fall through to the working dot, matching render.go's LivenessUnset arm.
    default:
      return Liveness.Running;
  }
}
function dotForLiveness(lv) {
  switch (lv) {
    case Liveness.Ready:
      return { glyph: READY_GLYPH, kind: "ready", spinning: false, label: "Ready" };
    case Liveness.Lost:
      return { glyph: LOST_GLYPH, kind: "lost", spinning: false, label: "Lost" };
    case Liveness.Dead:
      return { glyph: DEAD_GLYPH, kind: "dead", spinning: false, label: "Dead" };
    case Liveness.Archived:
      return { glyph: ARCHIVED_GLYPH, kind: "archived", spinning: false, label: "Archived" };
    case Liveness.LimitReached:
      return { glyph: LIMIT_GLYPH, kind: "limit", spinning: false, label: "Limit reached" };
    // LiveRunning and LivenessUnset both render as working (render.go:285, 297).
    case Liveness.Running:
    case Liveness.Unset:
    default:
      return WORKING;
  }
}
function isArchived(s) {
  return livenessOf(s) === Liveness.Archived;
}
function rowTitle(s) {
  const lv = livenessOf(s);
  const op = s.in_flight_op ?? InFlightOp.None;
  let title = s.title;
  if (op === InFlightOp.Killing || op === InFlightOp.Archiving) {
    title = "[deleting] " + title;
  } else if (lv === Liveness.Lost) {
    title = "[lost] " + title;
  } else if (lv === Liveness.LimitReached) {
    title = limitBadgePrefix(s) + title;
  }
  if (s.backend_type === "remote") {
    title = "[remote] " + title;
  }
  return title;
}
function limitBadgePrefix(s) {
  if (!s.limit_reset_at) {
    return "[limit] ";
  }
  const reset = new Date(s.limit_reset_at);
  if (Number.isNaN(reset.getTime())) {
    return "[limit] ";
  }
  return `[limit] resets ${formatLimitReset(reset, /* @__PURE__ */ new Date())} `;
}
function formatLimitReset(reset, now) {
  const h12 = (reset.getHours() + 11) % 12 + 1;
  const ampm = reset.getHours() < 12 ? "am" : "pm";
  const min = reset.getMinutes();
  const clock = min === 0 ? `${h12}${ampm}` : `${h12}:${String(min).padStart(2, "0")}${ampm}`;
  const sameDay = reset.getFullYear() === now.getFullYear() && reset.getMonth() === now.getMonth() && reset.getDate() === now.getDate();
  if (sameDay) {
    return clock;
  }
  const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  return `${months[reset.getMonth()]} ${reset.getDate()} ${clock}`;
}

// src/ui.ts
function h(tag, props = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key === "class") {
      el.className = value;
    } else {
      el[key] = value;
    }
  }
  for (const child of children) {
    el.append(child);
  }
  return el;
}
function orderedSessions(sessions) {
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
function render(root, state, actions) {
  root.replaceChildren(state.phase === "login" ? loginView(state, actions) : appView(state, actions));
}
function loginView(state, actions) {
  const input = h("input", {
    type: "password",
    id: "af-token",
    placeholder: "Paste your daemon token",
    autocomplete: "off",
    disabled: state.connecting
  });
  input.setAttribute("aria-label", "Daemon bearer token");
  const button = h(
    "button",
    { type: "submit", class: "af-primary", disabled: state.connecting },
    state.connecting ? "Connecting\u2026" : "Connect"
  );
  const form = h(
    "form",
    { class: "af-login-form" },
    h("label", { class: "af-field-label", htmlFor: "af-token" }, "Daemon token"),
    input,
    button
  );
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const token2 = input.value.trim();
    if (token2 !== "") {
      actions.connect(token2);
    }
  });
  const children = [
    h("h1", { class: "af-title" }, "Agent Factory"),
    h(
      "p",
      { class: "af-subtitle" },
      "Paste the daemon bearer token to connect. Get it from ",
      h("code", {}, "af token show"),
      " on the host."
    ),
    form
  ];
  if (state.loginError) {
    children.push(h("p", { class: "af-error", role: "alert" }, state.loginError));
  }
  return h("main", { class: "af-login" }, h("div", { class: "af-card" }, ...children));
}
function appView(state, actions) {
  const disconnect2 = h("button", { type: "button", class: "af-ghost" }, "Disconnect");
  disconnect2.addEventListener("click", () => actions.disconnect());
  const header = h(
    "header",
    { class: "af-appbar" },
    h("span", { class: "af-brand" }, "Agent Factory"),
    liveIndicator(state.live),
    disconnect2
  );
  const body = h("div", { class: "af-body" }, sidebar(state, actions), mainPane(state));
  return h("main", { class: "af-app" }, header, body);
}
function liveIndicator(live) {
  const label = live === "open" ? "Live" : live === "connecting" ? "Connecting\u2026" : "Reconnecting\u2026";
  const pip = h("span", { class: `af-live-pip af-live-${live}` });
  pip.setAttribute("aria-hidden", "true");
  const wrap = h("span", { class: "af-live" }, pip, h("span", { class: "af-live-label" }, label));
  wrap.setAttribute("role", "status");
  return wrap;
}
function sidebar(state, actions) {
  const count = state.sessions.length;
  const head = h(
    "div",
    { class: "af-rail-head" },
    h("span", { class: "af-rail-title" }, "Sessions"),
    h("span", { class: "af-rail-count" }, String(count))
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
        "."
      )
    );
  } else {
    for (const s of orderedSessions(state.sessions)) {
      list.append(sessionRow(s, s.title === state.selectedTitle, actions));
    }
  }
  return h("nav", { class: "af-rail" }, head, list);
}
function sessionRow(s, selected, actions) {
  const status = rowStatus(s);
  const dot = h(
    "span",
    { class: `af-dot af-dot-${status.kind}${status.spinning ? " af-dot-spin" : ""}` },
    status.glyph
  );
  dot.setAttribute("aria-hidden", "true");
  const title = h("div", { class: "af-row-title" }, rowTitle(s));
  const branch = h(
    "div",
    { class: "af-row-branch" },
    h("span", { class: "af-branch-icon" }, "\u2387"),
    " ",
    s.branch || "\u2014"
  );
  const cls = `af-row${selected ? " af-row-selected" : ""}${isArchived(s) ? " af-row-archived" : ""}`;
  const row = h("li", { class: cls }, dot, h("div", { class: "af-row-main" }, title, branch));
  row.setAttribute("role", "option");
  row.setAttribute("aria-selected", selected ? "true" : "false");
  row.setAttribute("title", `${s.title} \u2014 ${status.label}`);
  row.addEventListener("click", () => actions.select(s.title));
  return row;
}
function mainPane(state) {
  const selected = state.sessions.find((s) => s.title === state.selectedTitle) ?? null;
  if (!selected) {
    return h(
      "section",
      { class: "af-main af-main-empty" },
      h("p", { class: "af-empty-title" }, "Select a session"),
      h("p", { class: "af-empty-hint" }, "The terminal arrives in the next update.")
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
      `${status.label}${selected.branch ? ` \xB7 ${selected.branch}` : ""}`
    ),
    h("p", { class: "af-empty-hint" }, "The terminal for this session arrives in the next update.")
  );
}

// src/index.ts
var store = new Store({
  phase: "login",
  connecting: false,
  loginError: null,
  sessions: [],
  selectedTitle: null,
  live: "connecting"
});
var token = null;
var stream = null;
var resyncTimer = null;
function mount() {
  const root = document.getElementById("app");
  if (!root) {
    throw new Error("af-web: #app root element missing from index.html");
  }
  const rerender = () => render(root, store.get(), { connect, disconnect, select });
  store.subscribe(rerender);
  rerender();
  document.addEventListener("keydown", onKeydown);
  const existing = loadToken();
  if (existing) {
    void connect(existing);
  }
}
async function connect(candidate) {
  store.set({ connecting: true, loginError: null });
  let sessions;
  try {
    sessions = await probeToken(candidate);
  } catch (e) {
    clearToken();
    store.set({ phase: "login", connecting: false, loginError: describeError(e) });
    return;
  }
  token = candidate;
  storeToken(candidate);
  store.set({
    phase: "app",
    connecting: false,
    loginError: null,
    sessions,
    selectedTitle: pickSelection(sessions, store.get().selectedTitle),
    live: "connecting"
  });
  startStream(candidate);
}
function disconnect() {
  stopStream();
  token = null;
  clearToken();
  store.set({
    phase: "login",
    connecting: false,
    loginError: null,
    sessions: [],
    selectedTitle: null,
    live: "connecting"
  });
}
function select(title) {
  store.set({ selectedTitle: title });
}
function startStream(tok) {
  stopStream();
  stream = new EventStream(tok, {
    onEvent,
    onResync: requestResync,
    onStatus: (s) => store.set({ live: s })
  });
  stream.start();
}
function stopStream() {
  if (resyncTimer !== null) {
    window.clearTimeout(resyncTimer);
    resyncTimer = null;
  }
  if (stream) {
    stream.stop();
    stream = null;
  }
}
function onEvent(ev) {
  const { sessions, needsResync } = applyEvent(store.get().sessions, ev);
  store.set({ sessions, selectedTitle: pickSelection(sessions, store.get().selectedTitle) });
  if (needsResync) {
    requestResync();
  }
}
function requestResync() {
  if (resyncTimer !== null) {
    return;
  }
  resyncTimer = window.setTimeout(() => {
    resyncTimer = null;
    const tok = token;
    if (!tok) {
      return;
    }
    void fetchSnapshot(tok).then((sessions) => {
      store.set({ sessions, selectedTitle: pickSelection(sessions, store.get().selectedTitle) });
    }).catch(() => {
    });
  }, 150);
}
function onKeydown(e) {
  const state = store.get();
  if (state.phase !== "app") {
    return;
  }
  const target = e.target;
  if (target && (target.tagName === "INPUT" || target.tagName === "TEXTAREA")) {
    return;
  }
  let delta = 0;
  if (e.key === "ArrowDown" || e.key === "j") {
    delta = 1;
  } else if (e.key === "ArrowUp" || e.key === "k") {
    delta = -1;
  } else {
    return;
  }
  const ordered = orderedSessions(state.sessions);
  if (ordered.length === 0) {
    return;
  }
  e.preventDefault();
  const cur = ordered.findIndex((s) => s.title === state.selectedTitle);
  let next;
  if (cur === -1) {
    next = delta > 0 ? 0 : ordered.length - 1;
  } else {
    next = Math.min(Math.max(cur + delta, 0), ordered.length - 1);
  }
  const target2 = ordered[next];
  if (target2) {
    select(target2.title);
  }
}
function describeError(e) {
  if (e instanceof ApiError) {
    if (e.status === 401) {
      return "That token was rejected. Check `af token show` on the host and try again.";
    }
    if (e.status === 0) {
      return `Couldn't reach the daemon. Confirm the listener address and TLS, then retry. (${e.message})`;
    }
    return `Login failed: ${e.message}`;
  }
  return `Login failed: ${e.message}`;
}
if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", mount, { once: true });
} else {
  mount();
}
