package app

import "github.com/sachiniyer/agent-factory/session"

// The app test binary panics on illegal lifecycle transitions (#1195 Phase 2d):
// the TUI routes optimistic create/kill/archive overlays through the chokepoint,
// and a mis-ordered edge (e.g. a double BeginCreate, #1350) must be a loud red
// failure in tests rather than a silently-ignored soft error. Production leaves
// the hook nil (soft error only).
func init() {
	session.SetIllegalTransitionHook(func(msg string) { panic("app: illegal transition: " + msg) })
}
