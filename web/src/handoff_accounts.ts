import { accountChoices } from "./account_scope.js";
import type { AccountsResponse } from "./types.js";

/** Whether handing off a session that holds `currentAccount` must name a
 *  target account. Only a PINNED account does: one af chose itself — the
 *  create-time router's pick, `account_auto_selected` — is released by an
 *  agent-only handoff, which the daemon admits and clears in the swap
 *  (#4404 review). */
export function handoffAccountPinned(currentAccount: string | undefined, currentAccountAuto: boolean | undefined): boolean {
  return !!currentAccount && currentAccountAuto !== true;
}

export function handoffAccountChoices(accounts: AccountsResponse, agent: string, currentAccount = "") {
  const rows = accountChoices(accounts, agent);
  return accounts.entries.filter((entry) => entry.agent === agent && entry.name !== currentAccount && !entry.registration_only)
    .map((entry) => ({ ...rows.find((row) => row.value === entry.name)!, logged_in: entry.logged_in }));
}
