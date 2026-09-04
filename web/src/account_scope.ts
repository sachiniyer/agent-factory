// The create form's credential-account choice (#3844).
//
// The daemon has accepted an account on create since #3051 (the CLI's
// `--account`), and #3385 gave the web's Config view a section that registers and
// logs into one — but the new-session modal could not USE one, so the flow the web
// walks a user through ended at a terminal. This module is the web half of closing
// that: it turns the daemon's ListAccounts answer into the option list the modal
// renders, and it holds the create-time rules that answer differs from the Config
// view's registry list by.
//
// Four rules this file exists to hold. The first is the one every catalog module
// here holds; the rest are specific to identity:
//
//  1. THE WEB KNOWS NO ACCOUNT OR AGENT NAMES. No local list, no name→label map.
//     Every option is built from the response, so an account registered on the
//     daemon host a second ago is offered with no change to this file.
//  2. AN ACCOUNT BELONGS TO AN AGENT. claude's "work" and codex's "work" are
//     different identities in different registries, so the list follows the form's
//     PROGRAM. The Config view legitimately lists every account — it is a registry
//     view — but a create-time picker that did would be offering guaranteed
//     failures.
//  3. AN UNUSABLE ROW IS LISTED WITH ITS REASON, NOT HIDDEN, and the two unusable
//     shapes are not the same shape. A registration-only account cannot scope a
//     session at all and BLOCKS the submit, the way an unavailable backend does. A
//     not-logged-in one is a perfectly good choice that happens to have no
//     credential in it yet, so it is labelled and allowed: hiding it would hide the
//     account someone registered thirty seconds ago and is on their way to logging
//     into.
//  4. NO CREDENTIAL MATERIAL, IN EITHER DIRECTION. An account is a directory name.
//     Nothing here reads, transports or displays a secret; `logged_in` is a stat
//     the daemon took of the agent's own file, and af never opens it.

import type { ProgramCatalog } from "./programs.js";
import type { AccountsResponse, SessionData } from "./types.js";

/** The sentinel value of the "ambient identity" choice — the identity every
 *  session ran as before this field existed. It is the EMPTY STRING on purpose:
 *  it is what createSession omits on, so picking it sends no account and the
 *  agent's own ambient credential decides, exactly as leaving `--account` off
 *  does. Any non-empty sentinel here would eventually be sent as a literal
 *  account name. */
export const AMBIENT_ACCOUNT = "";

/** An account choice, ready to render as an <option>. */
export interface AccountChoice {
  /** The <option> value: AMBIENT_ACCOUNT, or an account name to send verbatim. */
  value: string;
  /** The visible label: the account name as the DAEMON spelled it, plus what the
   *  daemon said about its state. Never looked up in a local map. */
  label: string;
  /** The agent whose registry this account lives in, from the entry itself. */
  agent: string;
  /** Why a session cannot be scoped to this account at all; "" when it can. A
   *  non-empty value BLOCKS the submit — the picker must not promise an identity
   *  the create would refuse. */
  blocked: string;
  /** Something true about this choice that does NOT block it; "" when there is
   *  nothing to say. Kept separate from `blocked` because collapsing the two
   *  would force one of the two mistakes: blocking a usable account, or saying
   *  nothing about one that is about to run without a credential. */
  note: string;
}

/**
 * The agent whose account registry a create with this program selection would use.
 *
 * Both inputs are the daemon's own values: the program is a name from ListPrograms
 * (which is `tmux.SupportedPrograms`, whose entries ARE the agent names), and the
 * "repo default" selection resolves through the catalog's `default`, which the
 * daemon computed with the same precedence the create will apply. Returns "" when
 * nothing can be resolved — a repo-default selection against a catalog that
 * reported no default — and the caller must then offer no accounts rather than
 * guess a registry.
 *
 * What this deliberately does NOT try to do is resolve `program_overrides`. A
 * repo can point the label `claude` at another agent's binary, and only the daemon
 * can see that; it refuses such a create with its own message naming both agents
 * (session/instance_factory.go refuseUnsupportedAccountAgent). This resolves the
 * LABEL, which is exactly what `af sessions create --account` validates against,
 * so the two surfaces agree and the daemon remains the backstop for both.
 */
export function accountAgentFor(program: string, catalog: ProgramCatalog | null): string {
  const picked = program.trim();
  if (picked !== "") {
    return picked;
  }
  return catalog?.default ?? "";
}

/**
 * Whether the daemon's roster covers this agent at all.
 *
 * The roster is `ListAccountsResponse.agents`, which the daemon always sends in
 * FULL — deliberately not narrowed by the request — so this stays true the day a
 * fourth agent is verified, with no change here. An agent outside it (aider,
 * opencode) has no account registry, which is a fact about the agent rather than
 * a failure.
 */
export function accountAgentSupported(accounts: AccountsResponse | null, agent: string): boolean {
  if (accounts === null || agent === "") {
    return false;
  }
  return accounts.agents.includes(agent);
}

/**
 * Builds the account picker's choices from the daemon's registry, narrowed to one
 * agent.
 *
 * The first choice is always the ambient identity, so a user can see — and return
 * to — the default every create had before this field existed. The rest are the
 * daemon's entries FOR THIS AGENT, in the order it sent them.
 *
 * The narrowing is done here, over each entry's own `agent`, rather than by asking
 * the daemon to filter: that makes it a property of the picker instead of a promise
 * about the response, and the failure it prevents is the identity kind — a codex
 * account offered to a claude session is a create that fails, or worse, one that
 * quietly does not.
 */
export function accountChoices(accounts: AccountsResponse | null, agent: string): AccountChoice[] {
  const choices: AccountChoice[] = [
    { value: AMBIENT_ACCOUNT, label: "Ambient identity (the agent's own login)", agent, blocked: "", note: "" },
  ];
  if (accounts === null || agent === "") {
    return choices;
  }
  for (const entry of accounts.entries) {
    if (entry.agent !== agent) {
      continue;
    }
    const marks: string[] = [];
    if (entry.registration_only) {
      marks.push("registration only");
    }
    if (!entry.logged_in) {
      marks.push("not logged in");
    }
    choices.push({
      value: entry.name,
      label: marks.length === 0 ? entry.name : `${entry.name} — ${marks.join(" · ")}`,
      agent: entry.agent,
      // The daemon is the authority on this state: `registration_only` is computed
      // by the build that owns the registry, which may be newer than this client,
      // so the flag is trusted and only the wording is written here. The canonical
      // sentence lives in Go (internal/sessionenv AccountRegistrationOnlyReason)
      // and does not cross the wire; this says the same thing in the same terms,
      // and both are driven by the one flag.
      blocked: entry.registration_only
        ? `A session cannot be scoped to a ${entry.agent} account yet — af has not verified that the account `
          + `boundary can prove how it launches ${entry.agent}, so it refuses rather than risk starting the `
          + `session on the ambient identity. Registering and logging in work today.`
        : "",
      note: entry.logged_in
        ? ""
        : `${entry.name} has no ${entry.agent} credential yet — the session will run as it until you log in `
          + `from the Config view.`,
    });
  }
  return choices;
}

/**
 * The message to show for the currently-selected account, or "" when there is
 * nothing to say. The blocking reason wins over the informational note: a user
 * whose submit is disabled must be told why before being told anything else.
 */
export function accountNotice(choices: AccountChoice[], selected: string): string {
  const choice = choices.find((c) => c.value === selected);
  if (choice === undefined) {
    return "";
  }
  return choice.blocked !== "" ? choice.blocked : choice.note;
}

/**
 * Whether a create scoped to this selection can proceed.
 *
 * A choice the picker does not list at all is allowed through, for the reason
 * backendSelectable allows an unlisted backend: the daemon is the authority on what
 * it accepts, and a client vetoing a value it merely does not recognize would be
 * the hardcoded-enum bug wearing a different hat.
 */
export function accountSelectable(choices: AccountChoice[], selected: string): boolean {
  const choice = choices.find((c) => c.value === selected);
  return choice === undefined || choice.blocked === "";
}

/**
 * Compares the account that came BACK on the created session with the one that was
 * picked, and returns what to tell the user — "" when they agree.
 *
 * This is the version-skew guard. A daemon predating account support drops the
 * field silently, so the session runs on the ambient identity while the UI goes on
 * reporting the account the user chose: the silent-wrong-identity outcome the whole
 * feature exists to prevent, arriving through skew instead of through the
 * environment. `af sessions create` runs the same check (api/sessions.go), and so
 * does the TUI (app/account_picker.go accountSkewRefusal).
 *
 * It reads what came back rather than what was sent, because the daemon is the
 * authority on what it stored, and it names BOTH identities plus the session —
 * which now exists, and has to be removed rather than used.
 */
export function accountSkewMessage(requested: string, created: SessionData): string {
  const want = requested.trim();
  if (want === AMBIENT_ACCOUNT) {
    return "";
  }
  const got = (created.account ?? "").trim();
  if (got === want) {
    return "";
  }
  // Two shapes, two causes, two remedies. A DROPPED field is the version skew this
  // guard was built for; a field applied as some OTHER account is not skew at all —
  // the daemon knew the field and stored something else — so pointing at an upgrade
  // there would send the user to fix the wrong thing.
  if (got === "") {
    return `Session "${created.title}" was created but the daemon did not apply account "${want}" — it is running `
      + `on the ambient identity. The running daemon predates account support; upgrade it, then kill this session `
      + `and create it again.`;
  }
  return `Session "${created.title}" was created but the daemon applied account "${got}", not the "${want}" that `
    + `was picked — it is running as an identity you did not choose. Kill this session and create it again.`;
}
