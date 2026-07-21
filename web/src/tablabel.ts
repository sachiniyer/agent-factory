// What a tab READS AS — the one derivation shared by every surface that names a
// tab: the tab bar's buttons (ui.ts) and the split panes' headers (split.ts).
//
// It lives in its own css-free pure module, like layout.ts and tabaddr.ts and for
// the same reason: split.ts pulls in xterm and its CSS, which the node test runner
// cannot load, so a mapping the panes need could not otherwise be unit-tested. The
// deeper reason is #1813: the pane headers read "Tab 1/2/3" precisely BECAUSE the
// label lived in ui.ts where the panes couldn't reach it, so they invented an
// ordinal of their own. One exported derivation is what keeps the two surfaces from
// drifting again.
//
// The mapping mirrors the TUI (ui/tree/labels.go labelForTab) rather than forking
// it: the agent tab is always "Agent" and a shell tab always "Terminal", both
// IGNORING tab.name — which is exactly why those two kinds are not renameable (see
// isRenameableTab): a rename would change a name no surface renders.

import { TabKind } from "./types.js";
import type { IconName } from "./icon.js";

/** The fields any tab-shaped record must carry to be named. Structural, so both a
 *  wire TabData and split.ts's parallel kind/name arrays satisfy it. */
export interface NamedTab {
  name: string;
  kind: number;
}

/**
 * The per-kind icon a tab is prefixed with. It preserves the TUI's semantic groups
 * (agent, terminal/process, browser surface) while using the web's Lucide subset.
 *
 * An unknown kind takes the process icon, matching labelForTab's own default
 * branch (which names an unknown kind like a process).
 */
export function tabIcon(kind: number): IconName {
  switch (kind) {
    case TabKind.Agent:
      return "bot";
    case TabKind.Shell:
      return "terminal";
    case TabKind.Process:
      return "terminal";
    // VS Code (#1817) shares the web icon deliberately, on the same rule that has
    // shell and process share the terminal icon: the icon names what a tab IS, and a VS Code tab
    // is an embedded browser surface with no PTY — a web pane whose page happens to
    // be an editor. What separates them is the text beside the glyph ("VS Code" vs
    // the target's name), exactly as the command name separates a process tab from a
    // shell. Letting it fall through to the default arm would call it a terminal,
    // which is the one thing it is not.
    case TabKind.Web:
    case TabKind.VSCode:
      return "panels";
    default:
      return "terminal";
  }
}

/**
 * The label a tab reads as, mirroring the TUI's labelForTab (ui/tree/labels.go):
 * the agent tab is "Agent", a shell tab is "Terminal", a web tab shows its name (or
 * "Web"), and any other kind shows its name (or "Tab").
 *
 * Agent and shell deliberately IGNORE tab.name — the TUI does the same, and the
 * daemon refuses to rename them for it.
 */
export function tabLabel(tab: NamedTab): string {
  switch (tab.kind) {
    case TabKind.Agent:
      return "Agent";
    case TabKind.Shell:
      return "Terminal";
    case TabKind.Web:
      return tab.name || "Web";
    case TabKind.VSCode:
      return tab.name || "VS Code";
    default:
      return tab.name || "Tab";
  }
}

/** The tab's plain-text display name for titles and accessible names. The icon is
 *  deliberately absent: every visual instance is a decorative aria-hidden SVG, so
 *  assistive technology hears the meaningful label rather than an icon name. */
export function tabDisplayLabel(tab: NamedTab): string {
  return tabLabel(tab);
}

/**
 * Whether a tab's name is worth renaming — the gate on the web's rename affordance
 * (#1813), and the web mirror of session.TabKindRenameable (session/tab_arrange.go),
 * which is the daemon's own refusal.
 *
 * Web, process and VS Code tabs qualify, and the reason is tabLabel above rather
 * than any policy: an agent/shell tab renders a FIXED label and ignores its name, so
 * renaming one would silently change nothing a user can see. Offering the affordance
 * there could only produce a guaranteed-to-fail call (the daemon rejects it) or,
 * worse, a rename that appears to succeed and shows no result.
 *
 * VS Code (#1817) is renameable for exactly that reason and needed no new rule: it
 * renders `name || "VS Code"`, so the same "does this kind display its Name" test
 * that admits web and process admits it. If tabLabel ever starts reading `name` for
 * another kind, this must change with it — the two are one rule stated twice, and
 * the Go side keeps the same pairing beside its own label mapping.
 */
export function isRenameableTab(kind: number): boolean {
  return kind === TabKind.Web || kind === TabKind.Process || kind === TabKind.VSCode;
}
