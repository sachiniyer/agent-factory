//go:build !linux

package daemon

// unixSocketPathsOf cannot inspect another process's sockets off Linux —
// there is no /proc/<pid>/fd ↔ net/unix pair to read. ok=false is "no
// evidence", and socket evidence is the ONLY proof a tmux server can be a
// sandbox member: off Linux no server is ever proven private, and only
// recorded (pid, start id) pairs may be selected.
func unixSocketPathsOf(int) ([]string, bool) { return nil, false }
