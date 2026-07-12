// Entry point for the Phase-5 web client bundle (#1592). This PR ships only the
// protocol foundation — the wire-frame codec — so the bundle re-exports it and has
// no UI yet (xterm.js, the sidebar, and serving arrive in PR2+). esbuild bundles
// this into the committed dist/ that the daemon will later go:embed and serve.
export * from "./frame.js";
