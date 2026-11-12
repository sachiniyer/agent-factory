import { accountChoices } from "./account_scope.js";
import type { AccountsResponse } from "./types.js";

export function handoffAccountChoices(accounts: AccountsResponse, agent: string, currentAccount = "") {
  const rows = accountChoices(accounts, agent);
  return accounts.entries.filter((entry) => entry.agent === agent && entry.name !== currentAccount && !entry.registration_only)
    .map((entry) => ({ ...rows.find((row) => row.value === entry.name)!, logged_in: entry.logged_in }));
}

/** Load facts the handoff dialog's error line is derived from. */
export interface HandoffLoadState {
  catalogFailed: boolean;
  accountsFailed: boolean;
  /** The session runs under a named account, so targets must carry one. */
  scoped: boolean;
  /** Offerable targets, or null until both loads have settled. */
  targets: number | null;
}

/** What the handoff dialog's error line says about its two loads (#7102).
 *  The dialog asks ListPrograms and ListAccounts at once and recomputes this
 *  from every fact each time either settles, so the order they answer in cannot
 *  change the outcome. Before, whichever reply landed last wrote the line, and
 *  an agent list arriving after a registry failure cleared it.
 *
 *  An agent-list failure outranks the registry's: the ambient registry message
 *  promises a handoff that an unreadable catalog cannot offer. A registry failure
 *  outranks "no target", because an unreadable registry is why a scoped session
 *  has none — the registry is not empty, it could not be read. */
export function handoffLoadError(state: HandoffLoadState): string | null {
  if (state.catalogFailed) return "Could not load the agent list. Try again.";
  if (state.accountsFailed) {
    return state.scoped
      ? "Could not load accounts. Try again to choose a registered target account."
      : "Could not load accounts. You can still hand off to another agent using its ambient identity.";
  }
  if (state.targets === 0) {
    return state.scoped
      ? "No registered target account is available to hand off to."
      : "No other agent is available to hand off to.";
  }
  return null;
}
