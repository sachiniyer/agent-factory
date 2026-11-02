// The Mermaid library as its own lazy chunk, dist/lazy/mermaid.js (#6642):
// md_mermaid.ts fetches it only when a rendered doc holds a ```mermaid fence.
// It is bundled into the embedded dist rather than loaded from a CDN — af
// serves the web offline — and it renders in the daemon's control origin on
// semi-trusted (partly agent-authored) diagram source, so the config is
// locked down:
//
//   securityLevel 'strict'  label HTML is encoded, click statements are
//                           ignored, no script runs. `secure` (mermaid's own
//                           list) keeps a %%{init}%% directive from raising
//                           this or the maxEdges/maxTextSize ceilings.
//   suppressErrorRendering  a bad fence throws here and md_mermaid.ts draws
//                           the inline error — mermaid's own error graphic
//                           never reaches the DOM.
//   htmlLabels off          labels render as SVG <text>, so <foreignObject>
//                           never appears for the sanitizer to strip.
//
// renderMermaidDiagram's callers' output still passes through sanitizeSvg in
// md_mermaid.ts before it touches the document.

import mermaid from "mermaid";

let seq = 0;
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
    flowchart: { htmlLabels: false },
    markdownAutoWrap: false,
    maxTextSize: 50_000,
    maxEdges: 500,
  });
  const id = `af-mmd-${++seq}`;
  // A scratch element hosts the measuring div render() creates. It must be
  // ATTACHED — the diagram renderers look their svg up by document-wide
  // getElementById — but parked two viewports off-screen so nothing of
  // mermaid's is ever on the page. The finally drops whatever it leaves behind.
  const scratch = document.createElement("div");
  scratch.style.cssText = "position:fixed;left:-200vw;top:-200vh";
  document.body.appendChild(scratch);
  try {
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
