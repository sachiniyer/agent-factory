package session

import "errors"

// ErrAccountSwapAgentTeardownBlind means tab zero disappeared before its pane
// could be observed. An absent tmux binding cannot prove a detached writer is
// gone, so callers must retain an inert record rather than recover automatically.
var ErrAccountSwapAgentTeardownBlind = errors.New("agent vanished without its pane being observed; a detached child may still be writing the worktree")
