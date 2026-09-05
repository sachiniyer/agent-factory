package session

import "time"

// instanceNow is the mutation clock; tests replace it only outside parallel tests.
var instanceNow = time.Now

// touchLocked records a session state mutation. Caller holds i.mu for writing.
// Cache refreshes and derived projections must not call it.
func (i *Instance) touchLocked() {
	i.UpdatedAt = instanceNow()
}
