import { AMBIENT_ACCOUNT, accountChoices, accountDefaultFor } from "./account_scope.js";
import type { AccountsResponse } from "./types.js";

/** Keeps deliberate identity choices separate from the select's temporary rows. */
export class AccountSelection {
  picked = false;
  private agent = "";
  private value = AMBIENT_ACCOUNT;

  pick(value: string): void {
    this.picked = true;
    this.value = value;
  }

  render(accounts: AccountsResponse | null, agent: string, failed = false): string {
    // Loading rows are not evidence that a selected identity has disappeared.
    if (accounts === null || (!agent && this.namedChoicePending)) return AMBIENT_ACCOUNT;
    const rows = accountChoices(accounts, agent, failed);
    const changedAgent = agent !== this.agent;
    this.agent = agent;
    if (this.picked && (changedAgent || !rows.some(row => row.value === this.value))) {
      this.picked = false;
      this.value = AMBIENT_ACCOUNT;
      return AMBIENT_ACCOUNT;
    }
    const value = this.picked ? this.value : accountDefaultFor(accounts, agent);
    return rows.some(row => row.value === value) ? value : AMBIENT_ACCOUNT;
  }

  get namedChoicePending(): boolean {
    return this.picked && this.value !== AMBIENT_ACCOUNT;
  }
}
