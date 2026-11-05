//go:build !linux

package daemon

// unixSocketPathsOf cannot inspect another process's sockets off Linux —
// there is no /proc/<pid>/fd ↔ net/unix pair to read. ok=false is "no
// evidence": a non-tmux process falls back to environment binding alone,
// while a tmux server still needs a socket path provable from its argv
// before it can be a sandbox member.
func unixSocketPathsOf(int) ([]string, bool) { return nil, false }
