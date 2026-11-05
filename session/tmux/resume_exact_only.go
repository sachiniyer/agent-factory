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

// ProgramResumesConversation reports whether program — launched through the
// exact-resume rewrite a re-spawn applies — comes up on an existing
// conversation. af suppresses the shared-home "latest" rewrite for codex and
// amp, so one of those launches resumes only a conversation the program itself
// names; a codex selector af cannot fully model (--all drops the
// working-directory scope, an unclassified option leaves the target
// unprovable) does not count. Every other supported agent's resume form is
// scoped to the working directory, so its launch lands on the newest
// conversation in this session's own worktree — or the program already names
// one. An unrecognized command answers false: af cannot prove the process it
// launches resumes anything.
func ProgramResumesConversation(program string) bool {
	tokens, _ := SplitShellTokens(program)
	agentIdx, agent := findAgentToken(tokens)
	if agentIdx < 0 || agent == "" {
		return false
	}
	switch agent {
	case ProgramCodex:
		selector, resume := CodexResumeSelection(program)
		if !resume {
			return false
		}
		_, _, all, ok := CodexResumeTarget(selector)
		return ok && !all
	case ProgramAmp:
		_, alreadyResume := ampResumeInsertIndex(tokens, agentIdx)
		return alreadyResume
	case ProgramAider:
		for _, tok := range tokens[agentIdx+1:] {
			if tok == "--no-restore-chat-history" {
				return false
			}
		}
		return true
	}
	return true
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
