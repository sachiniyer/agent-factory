package daemon

import "github.com/sachiniyer/agent-factory/session"

// The daemon test binary panics on illegal lifecycle transitions (#1195 Phase
// 2d): the daemon is the authoritative writer routing archive/restore/create/
// liveness through the chokepoint, and a mis-ordered edge must be a loud red
// failure in tests rather than a silently-ignored soft error. Production leaves
// the hook nil (soft error only).
func init() {
	session.SetIllegalTransitionHook(func(msg string) { panic("daemon: illegal transition: " + msg) })
}
