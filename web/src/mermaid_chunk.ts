// The Mermaid library bundled into the render frame's own document,
// dist/lazy/mermaid-frame.html (#6642, #6669). md_mermaid.ts navigates a
// purpose-built iframe to that page — SANDBOXED without allow-same-origin, an
// opaque origin — rather than srcdoc-loading it: srcdoc inherits the page's
// CSP, and in WebKit 'self' matches nothing in an opaque origin so every load
// is denied. The library is INLINE in the document the daemon serves (script
// authorized by sha256, daemon/webserve.go), so the frame issues
// zero fetches of its own: nothing for an authenticating proxy to refuse,
// no CSP URL-source for an IPv6 or proxied origin to spell. Its CSP denies
// every fetch the diagram could name anyway (default-src 'none') — renderers
// can make the browser ask for URLs the diagram names long before the svg
// string reaches sanitizeSvg — and the svg comes out as a string, over
// postMessage, and is sanitized before it touches the real document.
//
// The config is still locked down too — defense in depth, not a substitute:
//
//   securityLevel 'strict'  label HTML is DOMPurify-cleaned, click
//                           statements are ignored, no script runs.
//   secure                  the default list keeps a %%{init}%% directive
//                           from touching securityLevel/maxEdges/
//                           maxTextSize; the additions keep it from
//                           turning label HTML back on or smuggling CSS
//                           (fontFamily/themeCSS/themeVariables) into the
//                           generated <style> or the host div's style
//                           attribute.
//   suppressErrorRendering  a bad fence throws here and md_mermaid.ts draws
//                           the inline error — mermaid's own error graphic
//                           never reaches the DOM.
//   htmlLabels off          labels render as SVG <text>; some renderers
//                           ignore it and emit foreignObject anyway, which
//                           is part of why the frame exists.

import mermaid from "mermaid";
import { decodeRenderRequest, encodeFrameReply, MSG_HELLO, MSG_READY, MSG_RENDERED, portGrantFor } from "./mermaid_wire.js";

// A5: belt for the module itself — no caller-driven runMermaid() ever.
mermaid.startOnLoad = false;

let queue: Promise<unknown> = Promise.resolve();

async function renderNow(source: string, theme: Record<string, string>): Promise<string> {
  // Config is global to the library and read inside the serialized render, so
  // it is re-applied for each call — cheap, and it keeps a theme change atomic
  // with the render that uses it.
  mermaid.initialize({
    startOnLoad: false,
    securityLevel: "strict",
    suppressErrorRendering: true,
    theme: "base",
    themeVariables: theme,
    // dagre, not the v12 elk default: the elk layout chunk alone is ~1.5MB and
    // dagre is the classic look every mermaid doc on the web has.
    layout: "dagre",
    // Labels as SVG <text>, never <foreignObject>: in v12 htmlLabels is a
    // global key (the flowchart-scoped one is deprecated and the v2 renderer
    // ignores it), and it rides the secure list so a %%{init}%% directive
    // cannot turn HTML labels back on — same for markdownAutoWrap.
    htmlLabels: false,
    flowchart: { htmlLabels: false },
    markdownAutoWrap: false,
    secure: [
      "secure", "securityLevel", "startOnLoad", "maxTextSize",
      "suppressErrorRendering", "maxEdges", "htmlLabels", "markdownAutoWrap",
      // B1(b): these feed generated CSS and the host div's style attribute —
      // a directive must never write into them.
      "fontFamily", "altFontFamily", "themeCSS", "themeVariables",
    ],
    maxTextSize: 50_000,
    maxEdges: 500,
  });
  // A random id, not a sequence: an id a doc could predict could collide with
  // getElementById lookups across renders. Math.random, not randomUUID — the
  // page may be served over plain http, where crypto.randomUUID is absent.
  const id = `af-mmd-${Math.random().toString(36).slice(2)}`;
  // A scratch element hosts the measuring div render() creates. It must be
  // ATTACHED — the diagram renderers look their svg up by document-wide
  // getElementById — but parked two viewports off-screen inside the render
  // frame, so nothing of mermaid's is ever on the page. The finally drops
  // whatever it leaves behind.
  const scratch = document.createElement("div");
  scratch.style.cssText = "position:fixed;left:-200vw;top:-200vh";
  document.body.appendChild(scratch);
  try {
    // A denied load (an img node under img-src 'none') can leave this promise
    // unsettled — mermaid's own serialized queue would then stay busy. The
    // timeout lives in the PARENT (md_mermaid.ts renderWithTimeout), which
    // tears the whole frame down so the next render gets a fresh queue; a
    // late finisher here writes only to the emptied scratch under a unique id.
    const { svg } = await mermaid.render(id, source, scratch);
    return svg;
  } finally {
    scratch.remove();
    document.getElementById(`d${id}`)?.remove();
  }
}

/** Renders `source` to its SVG string, serialized because mermaid's config is
 *  global: two renders must never interleave an initialize with another's run.
 *  bindFunctions is deliberately discarded — it is how click handlers would be
 *  attached, and diagrams in this view never get any. */
export function renderMermaidDiagram(source: string, theme: Record<string, string>): Promise<string> {
  const job = queue.then(() => renderNow(source, theme));
  queue = job.catch(() => {});
  return job;
}

// The module executes inside md_mermaid.ts's render frame document — an
// opaque origin (#6669) — so the window channel carries JSON strings only and
// exactly twice (mermaid_wire.ts): the ready announce out, the port grant in.
// The port then carries everything: a render request in (id + diagram source
// + theme), an SVG string or the error text back out. Renders only ever
// arrive on the granted port — a port lives in the DOCUMENT it was handed to
// and dies with it, so a document that replaces this one if the frame
// navigates itself can neither claim it nor have observed the hello nonce
// the parent waits to hear echoed back.
const hello = (() => {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
})();

// Announce the API is up — the parent answers with the port grant, and
// renders flow only after the hello echo proves the port landed in THIS
// document. "*" because the parent origin is unreachable from an opaque
// origin; the announce carries nothing but the nonce.
(window.parent as Window).postMessage(encodeFrameReply({ t: MSG_READY, hello }), "*");

window.addEventListener("message", (e) => {
  const port = portGrantFor(e, window.parent);
  if (!port) return;
  port.postMessage(encodeFrameReply({ t: MSG_HELLO, hello }));
  port.onmessage = (pe: MessageEvent) => {
    const req = decodeRenderRequest(pe.data);
    if (!req) return;
    void renderMermaidDiagram(req.source, req.theme).then(
      (svg) =>
        port.postMessage(encodeFrameReply({ t: MSG_RENDERED, id: req.id, ok: true, svg })),
      (err: unknown) =>
        port.postMessage(
          encodeFrameReply({
            t: MSG_RENDERED,
            id: req.id,
            ok: false,
            error: err instanceof Error ? err.message : String(err),
          }),
        ),
    );
  };
  port.start();
});
