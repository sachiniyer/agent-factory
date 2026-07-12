// Entry point for the Agent Factory web client (#1592 Phase 5). This PR (PR2)
// wires the serving shell + token login: load the page → paste the daemon token →
// probe an authed endpoint → on success render the authed (empty) app, on failure
// show an actionable error. The sidebar, terminal, and modals land in PR3+.
//
// The bundle is fully self-contained (esbuild output + the imported CSS, no CDN,
// no off-origin fetch) so the daemon's `Content-Security-Policy: default-src
// 'self'` holds — the only network calls are same-origin /v1/ requests.

import "./styles.css";
import { ApiError, clearToken, loadToken, probeToken, storeToken } from "./api.js";
import { Store } from "./store.js";
import { render, type AppState } from "./ui.js";

const store = new Store<AppState>({
  phase: "login",
  connecting: false,
  loginError: null,
});

function mount(): void {
  const root = document.getElementById("app");
  if (!root) {
    throw new Error("af-web: #app root element missing from index.html");
  }
  const rerender = () => render(root, store.get(), { connect, disconnect });
  store.subscribe(rerender);
  rerender();

  // Resume within the tab: sessionStorage keeps the token across a reload, so a
  // returning tab re-probes it silently and skips the login form on success.
  const existing = loadToken();
  if (existing) {
    void connect(existing);
  }
}

/** Validates the token (Snapshot probe), and on success persists it and shows the
 *  app; on failure clears it and surfaces an actionable error on the login view. */
async function connect(token: string): Promise<void> {
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

/** Forgets the token and returns to the login view. */
function disconnect(): void {
  clearToken();
  store.set({ phase: "login", connecting: false, loginError: null });
}

/** Turns a probe failure into a message that tells the operator what to fix. */
function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 401) {
      return "That token was rejected. Check `af token show` on the host and try again.";
    }
    if (e.status === 0) {
      return `Couldn't reach the daemon. Confirm the listener address and TLS, then retry. (${e.message})`;
    }
    return `Login failed: ${e.message}`;
  }
  return `Login failed: ${(e as Error).message}`;
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", mount, { once: true });
} else {
  mount();
}
