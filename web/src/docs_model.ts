// Pure decisions behind the Docs tab (docs_pane.ts): doc order, status copy, how
// a proposal.changed event affects the open tab, and where "next question" goes.

import type { SyncStatus } from "./docs_sync.js";
import { changeNamesDoc, type ProposalChange, type ProposalDocRef } from "./proposal_api.js";

/** The built-in templates, in the order the doc strip shows them. */
export const TEMPLATE_ORDER: readonly string[] = ["goals", "technical", "api", "ui", "future"];

function byTemplateThenName(a: string, b: string): number {
  const ia = TEMPLATE_ORDER.indexOf(a);
  const ib = TEMPLATE_ORDER.indexOf(b);
  if (ia !== ib) {
    if (ia === -1) return 1;
    if (ib === -1) return -1;
    return ia - ib;
  }
  return a < b ? -1 : a > b ? 1 : 0;
}

/** Docs in template order, then alphabetical. */
export function orderDocs(docs: readonly ProposalDocRef[]): ProposalDocRef[] {
  return [...docs].sort((a, b) => byTemplateThenName(a.name, b.name));
}

export function statusText(status: SyncStatus, detail = ""): string {
  switch (status) {
    case "loading":
      return "Loading…";
    case "saved":
      return "";
    case "pending":
    case "saving":
      return "Saving…";
    case "merged":
      return "Merged agent edits";
    case "conflict":
      return "Not saved";
    case "markers":
      return "Resolve the markers to save";
    case "recovered":
      return "Unsaved changes recovered";
    case "gone":
      return "Doc deleted · your changes are kept";
    case "error":
      return detail ? `Not saved · ${detail}` : "Not saved";
  }
}

export type ChangeAction =
  /** The open doc changed: let its DocSync reload or merge. */
  | "open-doc"
  /** Another known doc changed: refresh its question count. */
  | "other-doc"
  /** A doc this tab does not know changed, or the event names none: refresh the list. */
  | "doc-list"
  | "ignore";

export function changeAction(
  change: ProposalChange,
  slug: string,
  open: ProposalDocRef | null,
  docs: readonly ProposalDocRef[],
): ChangeAction {
  if (change.slug !== slug) return "ignore";
  if (change.doc === "") return "doc-list";
  if (open && changeNamesDoc(change, open)) return "open-doc";
  return docs.some((d) => changeNamesDoc(change, d)) ? "other-doc" : "doc-list";
}

/** Where "next question" goes from the open doc: on within it while there is a
 *  question after the cursor ("here"), else the next doc in strip order that has
 *  one, else wrapping inside the open doc. Null when no doc has a question. */
export function nextQuestionDoc(
  ordered: readonly ProposalDocRef[],
  open: ProposalDocRef | null,
  counts: ReadonlyMap<string, number>,
  moreAfterCursor: boolean,
): { doc: ProposalDocRef; here: boolean } | null {
  if (open && moreAfterCursor) {
    return { doc: open, here: true };
  }
  const start = open ? ordered.findIndex((d) => d.name === open.name) : -1;
  for (let step = 1; step <= ordered.length; step++) {
    const doc = ordered[(start + step + ordered.length) % ordered.length];
    if (doc && (counts.get(doc.name) ?? 0) > 0) {
      return { doc, here: open !== null && doc.name === open.name };
    }
  }
  return null;
}

export function questionCountLabel(n: number): string {
  return n === 1 ? "1 open question" : `${n} open questions`;
}
