// The account-login overlay (#3385): the login pane, streamed into the browser.
//
// Split from accounts.ts because it imports terminal.ts, which pulls in xterm's
// stylesheet and UMD bundle — neither of which plain node can load. accounts.ts is
// reached from config.ts and therefore from ui.ts, whose unit tests import it
// directly, so the renderer and the terminal have to be separable. Exactly the
// division config.ts and config_assistant.ts already keep.
//
// THE PROBLEM THIS SOLVES, and it is the one #3385 called load-bearing: the login
// child runs on the DAEMON host while the human is usually on a laptop over
// Tailscale. So the button does not "open a browser" — it opens the daemon-side
// pane here, and the device-code flow — which af now SELECTS for all three CLIs
// rather than relying on them to fall back to it (#3854) — is completed by the
// human reading the code and finishing it in their OWN browser. A flow that
// insists on opening a browser on the server is one nobody is sitting in front
// of, and that stays an `af accounts login` on the host.
//
// No credential crosses this stream that af has read: the bytes are the pane's, on
// their way to a terminal, exactly as they would be on the daemon host's screen.

import { h } from "./dom.js";
import { accountLoginStreamEndpoint } from "./stream_endpoint.js";
import { AttachTerminal, type TerminalStatus } from "./terminal.js";
import type { AccountLoginResponse } from "./types.js";

/** A live login overlay. close() tears the terminal down. */
export interface AccountLoginController {
  close(): void;
}

/**
 * The copy for a login that produced no pane to watch.
 *
 * `finished` is a real outcome and not an error: `codex login` against an account
 * that already holds a credential prints and exits, and af reports that by the
 * ACCOUNT's artifact rather than by the launch error. The two branches are the
 * two things that can mean, and neither is "something broke".
 */
export function loginWithoutPaneCopy(login: AccountLoginResponse): { status: string; detail: string } {
  if (login.logged_in) {
    return {
      status: "Logged in",
      detail:
        `${login.agent}'s login flow finished without needing the terminal, and ${login.name} holds a ` +
        `credential.` + (login.notices?.length ? ` ${login.notices.join(" ")}` : ""),
    };
  }
  return {
    status: "Not logged in",
    detail:
      `The ${login.agent} login flow ended without leaving a credential in ${login.name}, so the account is ` +
      `registered but not logged in. Try again, or run \`af accounts login ${login.agent} ${login.name}\` on ` +
      `the daemon host.`,
  };
}

/**
 * Opens the login pane in an overlay, mounted into `mountHost` (the shell's
 * persistent modal host).
 *
 * The spawn has already happened — the shell called AccountLogin and hands the
 * result in — so this only attaches. That split is deliberate: the spawn can
 * fail in ways the operator must see (an agent with no verified login flow, the
 * codex keyring collapse, a missing binary) and those belong on the section's own
 * status line, not behind an overlay that opens onto an error.
 *
 * `onClosed` fires once, after teardown, so the shell can drop its handle and
 * re-read the accounts — the login's whole point is a state change the section
 * has to reflect.
 */
export function openAccountLogin(opts: {
  token: string;
  mountHost: HTMLElement;
  login: AccountLoginResponse;
  onClosed?: () => void;
}): AccountLoginController {
  const { token, mountHost, login, onClosed } = opts;

  let closed = false;
  let term: AttachTerminal | null = null;

  const status = h("span", { class: "af-assistant-status" }, "Connecting…");
  status.setAttribute("role", "status");
  const closeBtn = h("button", { type: "button", class: "af-ghost af-assistant-close" }, "×");
  closeBtn.setAttribute("aria-label", "Close");

  const title = `${login.agent} · ${login.name}`;
  const head = h(
    "div",
    { class: "af-assistant-head" },
    h("span", { class: "af-assistant-title" }, title),
    status,
    closeBtn,
  );
  // What af is running, shown rather than asserted: the operator can see it is
  // the agent's own login command and not something af invented.
  const note = h(
    "p",
    { class: "af-assistant-note" },
    `Running ${login.program} on the daemon host with this account's credential directory. ` +
      "af never reads the credential. The pane prints a URL and a device code · sign in on any device, " +
      "then paste the code back here.",
  );
  const termHost = h("div", { class: "af-assistant-term" });
  const body = h("div", { class: "af-assistant-body" }, note, termHost);
  const panel = h("div", { class: "af-assistant-panel" }, head, body);
  panel.setAttribute("role", "dialog");
  panel.setAttribute("aria-modal", "true");
  panel.setAttribute("aria-label", `Log in to ${title}`);
  const overlay = h("div", { class: "af-assistant-overlay" }, panel);

  function close(): void {
    if (closed) {
      return;
    }
    closed = true;
    term?.dispose();
    term = null;
    overlay.remove();
    document.removeEventListener("keydown", onKey);
    onClosed?.();
  }

  function onKey(e: KeyboardEvent): void {
    if (e.key === "Escape") {
      e.preventDefault();
      close();
    }
  }

  closeBtn.addEventListener("click", close);
  overlay.addEventListener("mousedown", (e) => {
    if (e.target === overlay) {
      close();
    }
  });
  document.addEventListener("keydown", onKey);
  mountHost.append(overlay);

  term = new AttachTerminal(termHost, token, accountLoginStreamEndpoint(login.agent, login.name), {
    onStatus: (s: TerminalStatus) => {
      if (!closed) {
        status.textContent = loginTerminalStatusCopy(s, login);
      }
    },
    // One pane, and no session identity: there is no nav/attach mode to keep in
    // sync the way a session split has, and no delivery-hold lease to take —
    // a login pane is not a session, so nothing addresses it that way.
    onFocusChange: () => {},
  });
  term.focus();
  return { close };
}

/**
 * The status label for a login terminal.
 *
 * "exited" is the interesting one and it is why this is not the assistant's copy
 * reused: for a login, the pane ending is the FLOW ending, which is the normal
 * and expected outcome — not a stream that dropped. Saying "disconnected" there
 * would read as a failure at the exact moment the thing succeeded.
 */
export function loginTerminalStatusCopy(status: TerminalStatus, login: AccountLoginResponse): string {
  switch (status) {
    case "connecting":
      return "Connecting…";
    case "open":
      return login.reused ? "Joined the running login" : "Live";
    case "exited":
      return "The login flow ended — close this to see the account's state";
    default:
      return "Reconnecting…";
  }
}
