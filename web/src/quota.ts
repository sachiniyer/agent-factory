// The Usage section of the web config view (#2983): the daemon's QuotaReport,
// rendered.
//
// IT IS NOT CONFIG, for the same reason the Accounts section above it is not
// (#3385): there is no usage key anywhere in config/ — the report is computed
// from session records on the daemon host. So it is a labelled section under
// the manifest rather than a key row, its cells show a STATE rather than a
// value, and nothing here goes near SetConfigValue.
//
// The words arrive pre-rendered: the daemon runs the same records→quota.Build
// read `af quota` makes and sends the four columns as text, so the web cannot
// disagree with the CLI or the TUI's same section. The one formatting decision
// left to a client — emphasizing a parked session — comes from the structured
// limited_sessions count, never from parsing the daemon's sentence.
//
// CSP-safe like the rest of the client: createElement + addEventListener via
// h(), no innerHTML with markup.

import { h } from "./dom.js";
import type { QuotaAgentRow, RenderedAccount } from "./types.js";

export interface QuotaState {
  agents: QuotaAgentRow[];
  warnings: string[];
  /** The per-account evidence rows (#6776) — one row per registered codex
   *  account af recorded usage windows for. Empty (the absent-table shape)
   *  unless the [account_handoff] machinery is engaged or left a state file,
   *  AND at least one codex account is registered. The web renders the
   *  ACCOUNTS block whenever the daemon serves these, matching `af quota`. */
  accounts: RenderedAccount[];
  /** The served mover line — the auto-handoff mode plus any recorded halt
   *  (#6776: a halted mover is never silent). Empty under the absent-table
   *  condition, so the line never materializes unconfigured. */
  mover: string;
  /** false until the first read answers: a section that has not read yet must
   *  say it is connecting, not that there is nothing to report. */
  loaded: boolean;
  /** The read's own failure — rendered in the section, never swallowed into an
   *  empty list that would read as "no usage". */
  error: string;
}

export function emptyQuotaState(): QuotaState {
  return { agents: [], warnings: [], accounts: [], mover: "", loaded: false, error: "" };
}

/** The heading's note — the same two-axis answer `af quota` gives, said before
 *  the first row can suggest "not reported" is a zero. */
const QUOTA_NOTE =
  "Quota is what the provider reports — today “not reported” everywhere, af declining to guess a ceiling. " +
  "Observed is what af’s own sessions show. af quota prints the same report.";

/** The per-account evidence section's closing note — the same explanation
 *  `af quota`'s RenderAccountSection prints after the table, so the web
 *  cannot disagree with the CLI about what the ACCOUNTS block is. */
const QUOTA_ACCOUNTS_NOTE =
  "ACCOUNTS is what af recorded in each account’s own rollout files — observed evidence, never a provider claim. " +
  "It appears only when [account_handoff] mode is enabled (or its machinery left a state file), " +
  "and is codex-only today: no other agent writes an equivalent record. " +
  "mover: is the configured mode plus any halt the engine recorded.";

/** The per-account evidence block (#6776): the ACCOUNTS table the daemon
 *  pre-renders, mirroring `af quota`'s RenderAccountSection so the web shows the
 *  daemon's own words rather than re-deriving the vocabulary. Renders only when
 *  the daemon served accounts OR a mover line — the absent-table shape (neither)
 *  is no block at all, exactly like the CLI. Returns null for that shape so the
 *  caller appends nothing. */
function renderQuotaAccountsSection(state: QuotaState): HTMLElement | null {
  if (state.accounts.length === 0 && state.mover === "") {
    return null;
  }
  const block = h("div", { class: "af-quota-accounts" });
  block.setAttribute("aria-label", "Account evidence");
  block.append(h("div", { class: "af-quota-accounts-head" }, "Account evidence"));
  if (state.accounts.length === 0) {
    // The empty-table line the CLI prints: the handoff machinery is engaged
    // (the mover line below says so) but no codex account is registered to
    // record windows for. The "Account evidence" heading above already labels
    // the section, so this line names only the empty state.
    block.append(h("p", { class: "af-quota-accounts-empty" }, "(no codex accounts registered)"));
  } else {
    block.append(
      h("div", { class: "af-quota-accounts-header" }, "ACCOUNT  OBSERVED  WINDOWS  DETAIL"),
    );
    const list = h("div", { class: "af-quota-list" });
    for (const acct of state.accounts) {
      const row = h(
        "div",
        { class: "af-quota-row" },
        h("span", { class: "af-quota-agent af-quota-account-agent" }, `${acct.agent}/${acct.name}`),
        h("span", { class: "af-quota-cell" }, acct.observed),
        h("span", { class: "af-quota-cell" }, acct.windows),
      );
      list.append(row, h("p", { class: "af-quota-detail" }, acct.detail));
    }
    block.append(list);
  }
  // The mover line rides inside the accounts section, exactly as the CLI's
  // RenderAccountSection prints it after the table — a recorded halt is never
  // silent (R16) and the section the daemon sourced it for is where it belongs.
  if (state.mover !== "") {
    block.append(h("p", { class: "af-quota-note" }, `mover: ${state.mover}`));
  }
  block.append(h("p", { class: "af-quota-accounts-note" }, QUOTA_ACCOUNTS_NOTE));
  return block;
}

export function renderQuotaSection(state: QuotaState): HTMLElement {
  const section = h("section", { class: "af-quota" });
  section.setAttribute("aria-label", "Usage");

  if (state.loaded === false && !state.error) {
    section.append(h("p", {}, "Connecting…"));
    return section;
  }

  const head = h(
    "div",
    { class: "af-quota-head" },
    h("span", { class: "af-quota-title" }, "Usage"),
    h("span", { class: "af-view-count" }, String(state.agents.length)),
  );
  section.append(head, h("p", { class: "af-quota-note" }, QUOTA_NOTE));

  if (state.error !== "") {
    section.append(
      h("p", { class: "af-quota-error", role: "alert" }, `Usage limits could not be read: ${state.error}`),
    );
    return section;
  }

  // Completeness caveats under the note, before the rows: a skipped record may
  // be hiding exactly the parked session the report exists to find.
  for (const warning of state.warnings) {
    section.append(h("p", { class: "af-quota-warning", role: "alert" }, warning));
  }

  if (state.agents.length === 0) {
    section.append(
      h("p", { class: "af-quota-empty" }, "No agent CLIs are configured, so there is nothing to report."),
    );
  } else {
    const list = h("div", { class: "af-quota-list" });
    for (const agent of state.agents) {
      const row = h(
        "div",
        { class: "af-quota-row" },
        h("span", { class: "af-quota-agent" }, agent.program),
        h("span", { class: "af-quota-cell" }, agent.quota),
        h(
          "span",
          { class: agent.limited_sessions > 0 ? "af-quota-cell af-quota-limited" : "af-quota-cell" },
          agent.observed,
        ),
      );
      list.append(row, h("p", { class: "af-quota-detail" }, agent.detail));
    }
    section.append(list);
  }

  // The per-account evidence section (#6776): the ACCOUNTS block the daemon
  // pre-renders, carried after the agent table exactly like `af quota`'s
  // RenderAccountSection — including the mover line, which rides in the
  // accounts section rather than under the note so the web matches the CLI's
  // shape. Null (appended as nothing) under the absent-table condition.
  const accounts = renderQuotaAccountsSection(state);
  if (accounts !== null) {
    section.append(accounts);
  }
  return section;
}
