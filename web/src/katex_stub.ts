// Stand-in for the `katex` package inside the mermaid render frame (#6943).
// Mermaid calls katex.renderToString only when label text carries $$…$$ —
// flowchart nodes, sequence messages, any label-bearing diagram — and the
// package is ~0.27MB the frame parses on every page load for a feature
// almost no fence uses. Returning the TeX source means a $$ label renders
// its source instead of typeset math: the label still appears and every
// diagram still renders (the same trade mermaid's own "tiny" build makes).
export default {
  renderToString: (source: string): string => source,
};
