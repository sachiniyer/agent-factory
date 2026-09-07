import type { AccountsResponse } from "./types.js";

export function handoffAccountChoices(accounts: AccountsResponse, agent: string, currentAccount = "") {
  return accounts.entries.filter((entry) => entry.agent === agent && entry.name !== currentAccount && !entry.registration_only)
    .map((entry) => ({ value: entry.name, label: entry.name + (accounts.defaults?.[agent] === entry.name ? " (project default)" : "") }));
}
