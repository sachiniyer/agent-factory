// The Accounts section of the web config view (#3385) — the owner's ask, in his
// words: "I'd like to be able to do the logging in from the config tab of the TUI
// or the web", and then "can I click a button in config to spawn a tmux session
// with the login instead?".
//
// IT IS NOT CONFIG, AND IT MUST NOT LOOK LIKE IT. #3385 raised this as the
// question to settle before building: there is no account key anywhere in
// config/, and an account is a 0700 directory under <AF_HOME>/accounts/. So the
// section carries its own heading saying what these are, its rows show a STATE
// rather than a value, and neither verb here goes near SetConfigValue.
//
// WHAT CROSSES THIS TRANSPORT, which is the constraint the whole epic stands on
// (#3388): af sets one variable and runs the AGENT's own login flow. No
// credential material passes through af, and none passes through the browser.
// There is no field here that accepts a token, and a design in which the browser
// POSTs one to the daemon is out of scope by that standing constraint.
//
// THE THING THE WEB HAS TO SOLVE THAT THE TUI DOES NOT. The login child runs on
// the DAEMON host; the human is usually on a laptop over Tailscale. #3385 called
// this the load-bearing design problem, and it is why the button does not "open a
// browser": it opens the daemon-side pane in a terminal here, so the device-code
// flow — which af SELECTS for all three CLIs since #3854 rather than hoping they
// fall back to it on a headless host — is completed by the human reading the code
// and finishing it in their OWN browser. A flow that insists on opening a browser
// on the server is one nobody is sitting in front of, and that stays a
// `af accounts login` on the host.
//
// CSP-safe like the rest of the client: createElement + addEventListener via h(),
// no innerHTML with markup.
//
// The LOGIN PANE lives in account_login_overlay.ts, not here, and the split is
// load-bearing rather than tidiness: that overlay imports terminal.ts, which pulls
// in xterm's stylesheet and UMD bundle, and plain node cannot load either. This
// module is imported by config.ts and so by ui.ts, whose unit tests import it
// directly — the same reason config.ts and config_assistant.ts are two files.

import { h } from "./dom.js";
import type { AccountEntry } from "./types.js";

/** What the Accounts section can ask the shell to do. The shell owns the token,
 *  the refresh and the modal host, so the section reports intent and renders the
 *  outcome it is handed back — the same division the config rows keep. */
export interface AccountActions {
  /** Creates an account's credential directory without logging in. */
  register: (agent: string, name: string) => void;
  /** Runs the agent's own login flow for an account on the daemon host and opens
   *  its pane. */
  login: (agent: string, name: string) => void;
}

/** The outcome of the last account action, as the shell learned it. */
export interface AccountStatus {
  agent: string;
  name: string;
  /** What happened, in the daemon's own words where it had any. */
  message: string;
  /** True when `message` is a refusal. */
  error: boolean;
}

/** Everything the section renders. `agents` is separate from `entries` because a
 *  fresh install has no accounts and the register form still has to offer the
 *  agents one can be made for. */
export interface AccountsState {
  entries: AccountEntry[];
  agents: string[];
  /** Why the accounts could not be read, or "" when they were. A section that
   *  silently shows nothing is indistinguishable from "you have no accounts", and
   *  those need different actions from the operator. */
  error: string;
  status: AccountStatus | null;
}

/** The empty state, so the shell has one place to get it from. */
export function emptyAccountsState(): AccountsState {
  return { entries: [], agents: [], error: "", status: null };
}

/** Marks a register field with the agent it belongs to, so a rebuild can restore
 *  the draft and the focus to the same one. */
export const ACCOUNT_INPUT_ATTR = "data-account-input";

/** The heading's note — the answer to #3385's placement question, said before the
 *  first row can suggest these are settable keys. */
const ACCOUNTS_NOTE =
  "Agent identities, not config keys. af runs the agent's own login flow against a directory " +
  "and never reads, stores or forwards the credential. Signing in is a device code · the pane " +
  "prints a URL, you finish it in your own browser.";

/**
 * Renders the Accounts section into a single element the config view appends.
 *
 * A function rather than a class: it holds no state of its own. Everything it
 * draws comes from `state`, and the one piece of local state a form needs — the
 * text in a name field — is read at submit time from the input itself, so a
 * re-render caused by someone else's event cannot resurrect a stale draft.
 */
export function renderAccountsSection(state: AccountsState, actions: AccountActions): HTMLElement {
  const section = h("section", { class: "af-accounts" });
  section.setAttribute("aria-label", "Accounts");

  const head = h(
    "div",
    { class: "af-accounts-head" },
    h("span", { class: "af-accounts-title" }, "Accounts"),
    h("span", { class: "af-view-count" }, String(state.entries.length)),
  );
  section.append(head, h("p", { class: "af-accounts-note" }, ACCOUNTS_NOTE));

  if (state.error !== "") {
    section.append(
      h("p", { class: "af-accounts-error", role: "alert" }, `Accounts could not be read: ${state.error}`),
    );
    return section;
  }

  if (state.agents.length === 0) {
    section.append(
      h(
        "p",
        { class: "af-accounts-empty" },
        "This daemon reports no agents that support accounts.",
      ),
    );
    return section;
  }

  const list = h("div", { class: "af-accounts-list" });
  for (const agent of state.agents) {
    const mine = state.entries.filter((e) => e.agent === agent);
    list.append(h("div", { class: "af-accounts-agent" }, agent));
    for (const entry of mine) {
      list.append(renderAccountRow(entry, state.status, actions));
    }
    list.append(renderRegisterRow(agent, state.status, actions));
  }
  section.append(list);
  return section;
}

/** One account: what it is, whether it holds a credential, and the login button. */
function renderAccountRow(entry: AccountEntry, status: AccountStatus | null, actions: AccountActions): HTMLElement {
  const row = h("div", { class: "af-accounts-row" });
  row.setAttribute("data-account", `${entry.agent}/${entry.name}`);

  const stateLabel = entry.logged_in ? "Logged in" : "Not logged in";
  row.append(
    h(
      "div",
      { class: "af-accounts-label" },
      h("span", { class: "af-accounts-name" }, entry.name),
      // A claim about a FILE, not about a working identity: af answers it by stat
      // and never opens the credential, so a revoked or expired one still reads as
      // present. The copy says "logged in", never "valid".
      h("span", { class: entry.logged_in ? "af-accounts-state-on" : "af-accounts-state-off" }, stateLabel),
      h("span", { class: "af-accounts-dir" }, entry.dir),
    ),
  );

  const button = h(
    "button",
    { type: "button", class: "af-ghost af-accounts-login" },
    entry.logged_in ? "Log in again" : "Log in",
  );
  button.addEventListener("click", () => actions.login(entry.agent, entry.name));
  row.append(button);

  if (entry.registration_only) {
    row.append(
      h(
        "div",
        { class: "af-accounts-notice" },
        `A session cannot be scoped to a ${entry.agent} account yet — registering and logging in work.`,
      ),
    );
  }
  appendStatus(row, status, entry.agent, entry.name);
  return row;
}

/** The register form for one agent. */
function renderRegisterRow(agent: string, status: AccountStatus | null, actions: AccountActions): HTMLElement {
  const row = h("div", { class: "af-accounts-row af-accounts-register" });
  const input = h("input", {
    type: "text",
    class: "af-input af-accounts-input",
    placeholder: `new ${agent} account name`,
  }) as HTMLInputElement;
  input.setAttribute("aria-label", `New ${agent} account name`);
  // The agent this field belongs to, so a rebuild can hand back the half-typed
  // name and the focus to the SAME field (config.ts rerenderKeepingUserState).
  // Every render replaces these nodes, and a name lost mid-typing is the smaller
  // half of that: once focus falls back to <body> the app's document-level keys
  // treat the REST of what is typed as shortcuts.
  input.setAttribute(ACCOUNT_INPUT_ATTR, agent);

  const submit = () => {
    const name = input.value.trim();
    if (name === "") {
      // Nothing to ask for. Sending it would earn a refusal from the daemon, and
      // reporting that to someone who pressed enter on an empty box is noise
      // where doing nothing is the answer.
      return;
    }
    // The name is NOT validated here. agentaccount.ValidateName is the one rule
    // and it refuses where the directory is created; a second copy in the browser
    // is how a UI comes to accept a name the writer rejects — the same argument
    // the config form makes about the validator.
    actions.register(agent, name);
    input.value = "";
  };

  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      submit();
    }
  });
  const button = h("button", { type: "button", class: "af-ghost af-accounts-add" }, "Register");
  button.addEventListener("click", submit);

  row.append(h("div", { class: "af-accounts-label" }, input), button);
  // A registration's outcome belongs on the form that produced it, and the name
  // it was for is gone from the input by then — so it is matched on the agent
  // with an empty name, which is what the shell records for a register failure.
  appendStatus(row, status, agent, "");
  return row;
}

/** Appends the status line when it belongs to this row. */
function appendStatus(row: HTMLElement, status: AccountStatus | null, agent: string, name: string): void {
  if (status === null || status.agent !== agent || status.name !== name) {
    return;
  }
  if (status.error) {
    // The daemon's own refusal, verbatim.
    row.append(h("div", { class: "af-accounts-error", role: "alert" }, status.message));
    return;
  }
  row.append(h("div", { class: "af-accounts-echo" }, status.message));
}
