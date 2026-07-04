package store

import "github.com/sachiniyer/agent-factory/session"

// LessInstanceOrder is the sidebar/tree instance ordering (#1144): the reserved
// root agent (#1106) sorts first by IDENTITY, then non-root instances sort
// oldest-first by CreatedAt.
//
// Root pins by IsReservedTitle, NOT by being oldest, so a root re-ensure with a
// fresh CreatedAt after a tmux death (#1108 Lost → recreate) still lands on top.
// A Lost root still pins; a Lost non-root sorts by its CreatedAt like any other
// instance — Lost is not special to ordering. The Title tiebreak on equal
// CreatedAt makes the order total and deterministic, so two identical snapshots
// never jitter and re-sorting an already-sorted slice is a no-op.
func LessInstanceOrder(a, b *session.Instance) bool {
	aRoot := session.IsReservedTitle(a.Title)
	bRoot := session.IsReservedTitle(b.Title)
	if aRoot != bRoot {
		// Root before non-root, regardless of either's CreatedAt.
		return aRoot
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.Title < b.Title
}
