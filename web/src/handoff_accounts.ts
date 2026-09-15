import { accountChoices } from "./account_scope.js";
import type { AccountsResponse } from "./types.js";

export function handoffAccountChoices(accounts: AccountsResponse, agent: string, currentAccount = "") {
  const rows = accountChoices(accounts, agent);
  return accounts.entries.filter((entry) => entry.agent === agent && entry.name !== currentAccount && !entry.registration_only)
    .map((entry) => ({ ...rows.find((row) => row.value === entry.name)!, logged_in: entry.logged_in }));
}
