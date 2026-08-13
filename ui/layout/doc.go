// Package layout is the TUI layout engine for the multi-pane rewrite
// (docs/design/tui-rewrite.md, epic #1024).
//
// It is pure, self-contained infrastructure: the Rect/Point geometry core,
// the Grid region solver with the §2.6 degradation ladder, the Pane
// interface every workspace region implements (§2.2), the focus Ring
// (§2.3), and the shared exact-Rect ClampToRect sizing helper. Mouse
// hit-testing lives in the zones subpackage (§2.5).
//
// app/ consumes it: home.relayout (app/home_model.go) sets the Grid inputs,
// solves, and rebuilds the focus Ring, and the mouse handlers hit-test
// through the zones subpackage.
package layout
