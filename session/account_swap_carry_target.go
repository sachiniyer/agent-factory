package session

import (
	"errors"
	"io/fs"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// targetReason is every refusal a new carry's plan gives against the target
// account's home, after every check on the source: a Claude copy filed there
// under another spelling of the worktree's path (claudeTargetReason), then
// what the copy itself would refuse against the target's copy of the
// conversation file (targetCopyReason).
func (c *conversationCarry) targetReason() string {
	if c.agent == tmux.ProgramClaude {
		if reason := c.claudeTargetReason(); reason != "" {
			return reason
		}
	}
	return c.targetCopyReason()
}

// targetCopyReason is the refusal the copy would give against the target's
// copy of the conversation file, judged now by the copy's own decision
// (judgeCarryFile) and writing nothing. The copy runs only after the outgoing
// runtime has stopped, so a refusal found only then is a fresh start
// require_carry can no longer refuse; found here, before the move, it can
// (#7273). That covers a target copy that diverges from the source, and a
// non-empty one an empty source cannot vouch for.
//
// The source is still live, so the copy judges again at stop time: only what
// the source writes in between — bytes that turn an extending target copy
// into a diverging one, say — is left for the copy to find.
func (c *conversationCarry) targetCopyReason() string {
	src, srcInfo, err := openCarryArtifact(c.srcRoot, c.transcript)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		src = nil
	case err != nil:
		return c.copyRefusal(carryFailureAt("the conversation could not be read safely from the previous account", err))
	default:
		defer src.Close()
	}
	dst, dstInfo, err := openCarryArtifact(c.dstRoot, c.transcript)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		dst = nil
	case err != nil:
		return c.copyRefusal(carryFailureAt("the new account holds something other than a plain file where the conversation belongs", err))
	default:
		defer dst.Close()
	}
	if _, err := judgeCarryFile(src, srcInfo, dst, dstInfo, carryNewTranscript); err != nil {
		return c.copyRefusal(err)
	}
	return ""
}

// copyRefusal is the clause a copy refusal err gives, in the agent's terms.
func (c *conversationCarry) copyRefusal(err error) string {
	return carryFailureReason(c.inAgentTerms(err))
}
