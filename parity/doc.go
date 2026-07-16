// Package parity holds the cross-surface capability inventory and the drift
// check that enforces it.
//
// Agent Factory ships three clients over one daemon (#960, #1592): the TUI, the
// web UI, and the CLI. The promise is that they are the same product. In
// practice capabilities drift — one surface gains a verb or a create option and
// the others silently fall behind, and nobody notices until a user hits the
// missing thing.
//
// This package makes that drift a test failure instead of a support question.
// inventory.json is the checked-in table of every user-facing capability and
// which surfaces expose it; parity_test.go derives the real surfaces FROM CODE
// (the cobra tree, the daemon route catalog, the TUI binding table, and the
// web client's RPC call sites) and fails when a surface grows a capability that
// the inventory has no entry for.
//
// Deriving from code is the point: a hand-maintained table drifts, which is the
// same failure this package exists to catch. The only hand-maintained part is
// the per-capability VERDICT (real gap / deliberate / partial) — the judgment a
// machine cannot make.
package parity
