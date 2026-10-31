package tmux

import "github.com/sachiniyer/agent-factory/log"

// accountWideLatestResume reports whether program's agent resumes "the latest
// conversation" from a store shared by every session on the same account home,
// not from one scoped to the working directory. codex and amp are the two:
// their rewrite in resumeProgram would pick whichever sibling session wrote last.
func accountWideLatestResume(program string) bool {
	tokens, _ := SplitShellTokens(program)
	idx, agent := findAgentToken(tokens)
	return idx >= 0 && (agent == ProgramCodex || agent == ProgramAmp)
}

// resumeProgramExactOnly is resumeProgram without the account-wide "latest"
// forms: an agent that can only resume the latest conversation on its account is
// left unchanged, so it starts a fresh conversation unless the caller already
// put the session's own conversation id in the program. Agents whose latest is
// scoped to the working directory resume as before.
func resumeProgramExactOnly(program string) string {
	if accountWideLatestResume(program) {
		return program
	}
	return resumeProgram(program)
}

// SetExactResumeOnly makes this session's re-spawn resume only a conversation
// the caller named in the program (see exactResumeOnly).
func (t *TmuxSession) SetExactResumeOnly(on bool) {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	t.exactResumeOnly = on
}

// resumeRewrite is the rewrite Restore's re-spawn applies to the program.
func (t *TmuxSession) resumeRewrite() func(string) string {
	t.programMu.RLock()
	exact := t.exactResumeOnly
	program := t.program
	t.programMu.RUnlock()
	if !exact {
		return resumeProgram
	}
	if accountWideLatestResume(program) && resumeProgram(program) != program {
		log.WarningLog.Printf("tmux session %q: no recorded conversation id to resume; starting a fresh conversation rather than the latest one on a shared account home", t.sanitizedName)
	}
	return resumeProgramExactOnly
}
