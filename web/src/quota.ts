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
import type { QuotaAgentRow } from "./types.js";

export interface QuotaState {
  agents: QuotaAgentRow[];
  warnings: string[];
  /** false until the first read answers: a section that has not read yet must
   *  say it is connecting, not that there is nothing to report. */
  loaded: boolean;
  /** The read's own failure — rendered in the section, never swallowed into an
   *  empty list that would read as "no usage". */
  error: string;
}

export function emptyQuotaState(): QuotaState {
  return { agents: [], warnings: [], loaded: false, error: "" };
}

/** The heading's note — the same two-axis answer `af quota` gives, said before
 *  the first row can suggest "not reported" is a zero. */
const QUOTA_NOTE =
  "Quota is what the provider reports — today “not reported” everywhere, af declining to guess a ceiling. " +
  "Observed is what af’s own sessions show. af quota prints the same report.";

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
    return section;
  }

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
  return section;
}
