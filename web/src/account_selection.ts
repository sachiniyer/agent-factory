import { AMBIENT_ACCOUNT, AMBIENT_PIN_ACCOUNT, accountChoices, accountDefaultFor } from "./account_scope.js";
import type { AccountsResponse } from "./types.js";

/** Keeps deliberate identity choices separate from the select's temporary rows. */
export class AccountSelection {
  picked = false;
  private agent = "";
  private value = AMBIENT_ACCOUNT;
  /** Whether the last render could promise a pool pick: a loaded registry from
   *  a routing daemon, on a backend the daemon routes (#4404 review). */
  private routes = false;

  pick(value: string): void {
    this.picked = true;
    this.value = value;
  }

  render(accounts: AccountsResponse | null, agent: string, failed = false, backendScoped: boolean | null = true): string {
    // A pick is scoped to the agent it was made for: account names and the
    // ambient bit mean nothing across an agent switch, so drop it even while
    // the new agent's rows have not loaded — accounts === null is a loading
    // or failed state, never evidence the pick survived (#4404 review). A
    // stale pick here is worse than a lost one: ambient:true could serialize
    // for an agent the user never chose it under. An EMPTY agent is not a
    // switch at all — it is the unresolved state a program select shows while
    // its catalog loads — so it must neither clear the pick nor re-anchor the
    // agent the pick was made under.
    if (agent !== "") {
      if (agent !== this.agent) {
        this.picked = false;
        this.value = AMBIENT_ACCOUNT;
      }
      this.agent = agent;
    }
    this.routes = accounts !== null && !failed && accounts.pool_routing === true && backendScoped === true;
    // Loading rows are not evidence that a selected identity has disappeared.
    if (accounts === null || (!agent && this.namedChoicePending)) return AMBIENT_ACCOUNT;
    const rows = accountChoices(accounts, agent, failed, backendScoped);
    if (this.picked && !rows.some(row => row.value === this.value)) {
      this.picked = false;
      this.value = AMBIENT_ACCOUNT;
      return AMBIENT_ACCOUNT;
    }
    const value = this.picked ? this.value : accountDefaultFor(accounts, agent);
    return rows.some(row => row.value === value) ? value : AMBIENT_ACCOUNT;
  }

  get namedChoicePending(): boolean {
    // A "named" choice names an account in the registry — neither the routable
    // empty value nor the ambient pin does, so neither needs account rows to
    // stay meaningful while the registry reloads.
    return this.picked && this.value !== AMBIENT_ACCOUNT && this.value !== AMBIENT_PIN_ACCOUNT;
  }

  /** The identity triple a create sends for the RETAINED pick — not whatever
   *  the DOM select currently shows. The distinction is load-bearing: a failed
   *  account reload replaces the select with a single "Accounts unavailable"
   *  row whose value is "", and serializing THAT while an ambient pin is
   *  retained would drop the pin the user chose — the wrong-identity outcome
   *  in miniature (#4404 review). */
  wireAccount(): { account: string; accountAmbient: boolean; accountAuto: boolean } {
    if (this.picked && this.value === AMBIENT_PIN_ACCOUNT) {
      return { account: AMBIENT_ACCOUNT, accountAmbient: true, accountAuto: false };
    }
    if (this.picked && this.value !== AMBIENT_ACCOUNT) {
      return { account: this.value, accountAmbient: false, accountAuto: false };
    }
    // Untouched field or the routable first row: the routable ask — made only
    // when the last render SAW a routing daemon and a backend it routes. A
    // pre-router daemon rejects the unknown field outright (the web sends no
    // client-version header, so it decodes strictly), and a failed or pending
    // catalog is not evidence of a router; either way the row was labelled
    // with the ambient/default contract, which is what omitting it asks for
    // (#4404 review).
    return { account: AMBIENT_ACCOUNT, accountAmbient: false, accountAuto: this.routes };
  }
}
