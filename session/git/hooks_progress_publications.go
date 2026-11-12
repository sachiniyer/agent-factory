package git

import "sync"

// hookProgressPublications counts the journals this process has renamed onto
// each journal name. Only a daemon publishes journals (a run without a scope
// prefix never writes one), and one daemon runs at a time, so once this daemon
// arms an abandonment, any replacement at that journal path is published
// through publishHookProgress in this process. An unchanged count therefore
// proves the journal now at the path is the one that was there when the
// abandonment was armed. That is the proof a retry needs when no read before
// arming decoded the journal's identity.
//
// Keyed by journal name, the hash of the worktree path: publication renames
// through the resolved AF home while abandonment spells the path through
// config, and the name is the same either way. A count only grows. A key
// shared by two homes (tests) can only raise it, which makes a retry leave a
// journal alone, never finish one it should not. One entry per worktree path
// this daemon has published a journal for.
var hookProgressPublications = struct {
	sync.Mutex
	byName map[string]uint64
}{byName: make(map[string]uint64)}

func recordHookProgressPublication(name string) {
	hookProgressPublications.Lock()
	defer hookProgressPublications.Unlock()
	hookProgressPublications.byName[name]++
}

// hookProgressPublicationCount reads the count for worktreePath's journal. It
// does no I/O, so restore can take it while arming without spending budget.
func hookProgressPublicationCount(worktreePath string) uint64 {
	hookProgressPublications.Lock()
	defer hookProgressPublications.Unlock()
	return hookProgressPublications.byName[hookProgressName(worktreePath)]
}
