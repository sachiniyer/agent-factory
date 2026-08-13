// The pane header's PR link (#3285): the web rendering of the pr_info
// projection the daemon now keeps current on its own (#3232). Pulled out as a
// pure decision, the tablabel.ts way, because every part of it can be wrong
// silently — a badge for a PR with nowhere to open, a stale text after a merge,
// a DOM write per snapshot for an unchanged badge.

import type { PRInfoData } from "./types.js";

/** What the header's PR link should show for one session's pr_info. */
export interface PRLinkView {
  /** False when there is nothing to open — no PR, or a record without a URL. */
  visible: boolean;
  /** "PR #41 · open" — sentence case, ` · ` joining number and state. */
  text: string;
  href: string;
  /** Hover text: the PR's own title when known. */
  title: string;
  /** Content signature: patching skips every DOM write while it is unchanged,
   *  so a snapshot tick with the same badge never touches the header. */
  sig: string;
}

const hidden: PRLinkView = { visible: false, text: "", href: "", title: "", sig: "" };

export function prLinkView(pr: PRInfoData | undefined): PRLinkView {
  // A record without a number is the daemon's "no PR" projection; a record
  // without a URL has nothing a link could open — treat both as absent rather
  // than rendering a dead control.
  if (!pr || !pr.number || !pr.url) {
    return hidden;
  }
  const state = (pr.state ?? "").toLowerCase();
  return {
    visible: true,
    text: state ? `PR #${pr.number} · ${state}` : `PR #${pr.number}`,
    href: pr.url,
    title: pr.title ?? "",
    sig: `${pr.number}|${state}|${pr.url}|${pr.title ?? ""}`,
  };
}
