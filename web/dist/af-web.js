// src/api.ts
var TOKEN_KEY = "af.token";
function loadToken() {
  return sessionStorage.getItem(TOKEN_KEY);
}
function storeToken(token) {
  sessionStorage.setItem(TOKEN_KEY, token);
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
async function af(method, body, token) {
  let resp;
  try {
    resp = await fetch(`/v1/${method}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`
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
function probeToken(token) {
  return af("Snapshot", { repo_id: "" }, token);
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
function render(root, state, actions) {
  root.replaceChildren(state.phase === "login" ? loginView(state, actions) : appView(actions));
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
    const token = input.value.trim();
    if (token !== "") {
      actions.connect(token);
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
function appView(actions) {
  const disconnect2 = h("button", { type: "button", class: "af-ghost" }, "Disconnect");
  disconnect2.addEventListener("click", () => actions.disconnect());
  const header = h(
    "header",
    { class: "af-appbar" },
    h("span", { class: "af-brand" }, "Agent Factory"),
    disconnect2
  );
  const body = h(
    "section",
    { class: "af-empty" },
    h("p", { class: "af-empty-title" }, "Connected."),
    h("p", { class: "af-empty-hint" }, "The session list and terminal arrive in the next update.")
  );
  return h("main", { class: "af-app" }, header, body);
}

// src/index.ts
var store = new Store({
  phase: "login",
  connecting: false,
  loginError: null
});
function mount() {
  const root = document.getElementById("app");
  if (!root) {
    throw new Error("af-web: #app root element missing from index.html");
  }
  const rerender = () => render(root, store.get(), { connect, disconnect });
  store.subscribe(rerender);
  rerender();
  const existing = loadToken();
  if (existing) {
    void connect(existing);
  }
}
async function connect(token) {
  store.set({ connecting: true, loginError: null });
  try {
    await probeToken(token);
  } catch (e) {
    clearToken();
    store.set({ phase: "login", connecting: false, loginError: describeError(e) });
    return;
  }
  storeToken(token);
  store.set({ phase: "app", connecting: false, loginError: null });
}
function disconnect() {
  clearToken();
  store.set({ phase: "login", connecting: false, loginError: null });
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
