// Opens OSC 8 hyperlinks in the web terminal without a confirm() dialog.
//
// OSC 8 is the escape sequence a program prints to make terminal text a link.
// With no linkHandler, xterm.js asks "Do you want to navigate to …? WARNING:
// This link could potentially be dangerous" on every click. Agents print these
// links routinely (PR URLs, docs), so the dialog was a warning the user had to
// dismiss each time. We open the link directly instead, but only for http and
// https URLs, and with noopener so the new tab cannot reach back into af.
//
// An OSC 8 link's visible text can differ from its target. The dialog used to
// be the only place the real URL showed, so hover now shows it as a tooltip.

import type { ILinkHandler } from "@xterm/xterm";
import { safeHttpUrl } from "./url.js";

/** The opener seam: window.open in the browser, a spy in tests. */
export type OpenWindow = (url: string, target: string, features: string) => unknown;

/** Shows the hovered link's real URL; "" clears it. */
export type SetTooltip = (tip: string) => void;

/** A linkHandler that opens safe links in a new tab and drops the rest. */
export function terminalLinkHandler(open: OpenWindow, setTooltip: SetTooltip): ILinkHandler {
  return {
    activate(_event, text) {
      const href = safeHttpUrl(text);
      if (href) open(href, "_blank", "noopener,noreferrer");
    },
    hover(_event, text) {
      // The normalized href, not the raw text: URL() turns a lookalike Unicode
      // host into its xn-- punycode form, so the tooltip shows what opens.
      setTooltip(safeHttpUrl(text) ?? "");
    },
    leave() {
      setTooltip("");
    },
  };
}
