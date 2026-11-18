//go:build !linux

package daemon

import "github.com/sachiniyer/agent-factory/internal/proctree"

// unixSocketsOf cannot inspect another process's sockets off Linux here: the
// throwaway proof stays Linux-only (#7536). ok=false is "no evidence", and
// socket evidence is the ONLY proof a tmux server can be a sandbox member: off
// Linux no server is ever proven private, and only recorded (pid, start id)
// pairs may be selected.
func unixSocketsOf(int) ([]proctree.UnixSocket, bool) { return nil, false }
