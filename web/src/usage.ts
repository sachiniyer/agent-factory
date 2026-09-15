// The Usage section of the web config view (#4361) — the browser face of
// `af quota`, rendered from the daemon's QuotaReport so all three surfaces
// (CLI, TUI, web) show the same wording of the same evidence.
//
// READ-ONLY, AND NOT CONFIG. Usage is evidence about the daemon host's
// sessions — what af has seen parked at a usage-limit wall, and when it saw it —
// not a setting this form can change. It therefore gets its own section and
// row shape rather than rendering like a key, and it sits above Accounts: both
// are daemon-reported state rather than config entries, and "is anything
// parked?" is the answer an operator opening this view after a dispatch failure
// is looking for.
//
// The section holds no projection of its own. The daemon renders the rows —
// QuotaRow is the wire shape — so nothing here re-derives a verdict, and the
// note and caveats ride the same response: the note is the standing "QUOTA is
// what the provider reports, OBSERVED is what af saw" framing, and a caveat is
// an under-read warning that must never let a partial report look complete.
//
// CSP-safe like the rest of the client: createElement + addEventListener via
// h(), no innerHTML with markup.

import { h } from "./dom.js";
import type { QuotaRow } from "./types.js";

/** Everything the section renders. */
export interface UsageState {
  /** False until the first read lands — the section shows its loading line
   *  rather than flashing an empty one. */
  loaded: boolean;
  rows: QuotaRow[];
  /** The standing QUOTA/OBSERVED framing, from the daemon's report. */
  note: string;
  /** Under-read warnings, from the daemon's report. */
  caveats: string[];
  /** Why the report could not be read, or "" when it was. A silently empty
   *  section reads as "no limits anywhere" — the confident answer a failed
   *  read must never produce. */
  error: string;
}

/** The empty state, so the shell has one place to get it from. */
export function emptyUsageState(): UsageState {
  return { loaded: false, rows: [], note: "", caveats: [], error: "" };
}

/**
 * Renders the Usage section into a single element the config view inserts above
 * Accounts. A function rather than a class: it holds no state of its own.
 */
export function renderUsageSection(state: UsageState): HTMLElement {
  const section = h("section", { class: "af-usage" });
  section.setAttribute("aria-label", "Usage");

  if (!state.loaded && state.error === "") {
    section.append(h("p", { class: "af-usage-note" }, "Loading usage…"));
    return section;
  }
  const head = h(
    "div",
    { class: "af-usage-head" },
    h("span", { class: "af-usage-title" }, "Usage"),
    h("span", { class: "af-view-count" }, String(state.rows.length)),
  );
  section.append(head);

  if (state.error !== "") {
    section.append(
      h("p", { class: "af-usage-error", role: "alert" }, `The usage report could not be read: ${state.error}`),
    );
    return section;
  }

  if (state.rows.length === 0) {
    section.append(
      h("p", { class: "af-usage-note" }, "No agent CLIs are configured, so there is nothing to report."),
    );
    return section;
  }

  // The framing first, before the first row can suggest otherwise: what the
  // provider reports is not what af has observed, and neither column is the
  // other.
  if (state.note !== "") {
    section.append(h("p", { class: "af-usage-note" }, state.note));
  }

  const list = h("div", { class: "af-usage-list" });
  for (const row of state.rows) {
    list.append(renderUsageRow(row));
  }
  section.append(list);

  for (const caveat of state.caveats) {
    section.append(h("p", { class: "af-usage-error", role: "alert" }, `warning: ${caveat}`));
  }
  return section;
}

/** One report row: the agent, its two verdicts, and the detail sentence —
 *  reset time and observation time — under it. */
function renderUsageRow(row: QuotaRow): HTMLElement {
  const el = h("div", { class: "af-usage-row" });
  el.setAttribute("data-agent", row.agent);
  el.append(
    h(
      "div",
      { class: "af-usage-label" },
      h("span", { class: "af-usage-agent" }, row.agent),
      h("span", { class: "af-usage-verdict" }, `${row.quota} · ${row.observed}`),
    ),
  );
  if (row.detail !== "") {
    el.append(h("div", { class: "af-usage-detail" }, row.detail));
  }
  return el;
}
