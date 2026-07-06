package tmux

// Program-command accessors for TmuxSession.
//
// program is the command the pane runs (override-resolved, flag-injected). It
// is not immutable: Restore() rewrites it (resume-flag injection, #595) on the
// restore goroutine while the status-poll and prompt-delivery goroutines read
// it (readiness heuristics, submit routing). Every access therefore goes
// through these helpers under programMu — the codex submit path reading program
// is what first exposed this data race under `go test -race` (#1254).

// SetProgram updates the program command before the session is started.
func (t *TmuxSession) SetProgram(program string) {
	t.setProgramCmd(program)
}

// Program returns the command this session's pane runs — after SetProgram,
// the override-resolved, flag-injected string. This is the ground truth for
// agent detection (DetectAgentFromCommand): what actually runs in the pane,
// as opposed to the config-name enum the instance was created with (#1116).
func (t *TmuxSession) Program() string {
	return t.programCmd()
}

// programCmd returns the pane's program command string under programMu.
func (t *TmuxSession) programCmd() string {
	t.programMu.RLock()
	defer t.programMu.RUnlock()
	return t.program
}

// setProgramCmd stores the pane's program command string under programMu.
func (t *TmuxSession) setProgramCmd(program string) {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	t.program = program
}
