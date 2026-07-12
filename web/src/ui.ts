// The view layer of the web client shell (#1592 Phase 5 PR2). It renders exactly
// two views into #app: the paste-token login (design §1.2) and the authed — but
// still empty — app shell. The session sidebar, terminal, and modals arrive in
// PR3+; this PR proves the serving + auth handoff with zero session UI.
//
// Rendering is direct DOM via a tiny `h()` helper (no framework, design §3.1) and
// is strictly CSP-safe: no inline scripts, no inline event handlers, no innerHTML
// with markup — everything is createElement + addEventListener, so the served
// `Content-Security-Policy: default-src 'self'` holds.

/** The whole client state for PR2: which view to show plus the login details. */
export interface AppState {
  phase: "login" | "app";
  /** true while a token probe is in flight (disables the login form). */
  connecting: boolean;
  /** an actionable message shown under the login form after a failed probe. */
  loginError: string | null;
}

/** Callbacks the shell invokes; index.ts owns the real behavior. */
export interface Actions {
  connect(token: string): void;
  disconnect(): void;
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

/** Renders the current state into the given root, replacing its contents. */
export function render(root: HTMLElement, state: AppState, actions: Actions): void {
  root.replaceChildren(state.phase === "login" ? loginView(state, actions) : appView(actions));
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

function appView(actions: Actions): HTMLElement {
  const disconnect = h("button", { type: "button", class: "af-ghost" }, "Disconnect");
  disconnect.addEventListener("click", () => actions.disconnect());

  const header = h(
    "header",
    { class: "af-appbar" },
    h("span", { class: "af-brand" }, "Agent Factory"),
    disconnect,
  );

  const body = h(
    "section",
    { class: "af-empty" },
    h("p", { class: "af-empty-title" }, "Connected."),
    h("p", { class: "af-empty-hint" }, "The session list and terminal arrive in the next update."),
  );

  return h("main", { class: "af-app" }, header, body);
}
