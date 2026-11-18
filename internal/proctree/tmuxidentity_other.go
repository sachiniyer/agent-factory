//go:build !linux

package proctree

// lookupForkShape is Lookup with an unknown fork shape: no other backend reads
// it. tmuxServerIdentity then refuses every process with a tmux parent, which
// on darwin the daemon(3) rule has already refused: a server's child has the
// server, not pid 1, as its parent.
func lookupForkShape(pid int) (Process, forkShape, error) {
	p, err := Lookup(pid)
	return p, forkShape{}, err
}
