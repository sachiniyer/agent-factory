package session

import (
	"errors"
	"io/fs"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
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
//
// The judgement opens and reads both accounts' homes, so it runs in a paced
// flight whose bound is per syscall: one that does not answer refuses as
// unverified (#7383). It classifies its own failures: only a look that saw
// what sits at the path keeps a name — "the new account holds something
// other than a plain file where the conversation belongs", or a refused
// link's own — and every other failed open, stat or read, on either account,
// is the could-not-look clause itself, never evidence about the copies
// (#7389's site-2 mapping, #7539 review R1).
func (c *conversationCarry) targetCopyReason() string {
	srcRoot, dstRoot, rel := c.srcRoot, c.dstRoot, c.transcript
	judged, err := fsprobe.Paced("carry target-copy judgement", srcRoot.key()+"\x00"+dstRoot.key()+"\x00"+rel,
		func(step func(), stat fsprobe.RegularStatFunc) (targetCopyJudgement, error) {
			return judgeTargetCopy(pacedCarryWalk(step, stat), srcRoot, dstRoot, rel), nil
		})
	switch {
	case err != nil:
		return carryUnlookedReason
	case judged.refusal == nil:
		return ""
	}
	return c.copyRefusal(judged.refusal)
}

// targetCopyJudgement is judgeTargetCopy's answer: the copy's refusal, or nil.
type targetCopyJudgement struct {
	refusal error
}

// judgeTargetCopy is the copy's decision on rel in srcRoot and dstRoot, made
// from the two open files without writing anything. Only a look that saw
// what it judged keeps a name: a refused link, on either account, and the
// guarded open's typed non-regular refusal at the target. Every other failed
// open, stat or read — the source's open among them — and a comparison the
// prefix read could not finish is a look that did not happen: its refusal is
// the could-not-look clause itself, never evidence about the copies (#7389's
// site-2 mapping, #7539 review R1).
func judgeTargetCopy(p carryPace, srcRoot, dstRoot carryRoot, rel string) targetCopyJudgement {
	src, srcInfo, err := openCarryArtifact(p, srcRoot, rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		src = nil
	case err != nil:
		if path := carrySymlinkPath(err); path != "" {
			return targetCopyJudgement{carryFailure(symlinkCarryReason(path), err)}
		}
		return targetCopyJudgement{carryFailure(carryUnlookedReason, err)}
	default:
		defer src.Close()
	}
	dst, dstInfo, err := openCarryArtifact(p, dstRoot, rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		dst = nil
	case err != nil:
		if refusal, named := carryTargetOpenRefusalAt(err); named {
			return targetCopyJudgement{refusal}
		}
		return targetCopyJudgement{carryFailure(carryUnlookedReason, err)}
	default:
		defer dst.Close()
	}
	if _, err := judgeCarryFile(p, src, srcInfo, dst, dstInfo, carryNewTranscript); err != nil {
		var carryErr *carryError
		inner := err
		if errors.As(err, &carryErr) {
			inner = carryErr.err
		}
		if carryLookupUnlooked(inner) {
			return targetCopyJudgement{carryFailure(carryUnlookedReason, err)}
		}
		return targetCopyJudgement{err}
	}
	return targetCopyJudgement{}
}

// copyRefusal is the clause a copy refusal err gives, in the agent's terms.
func (c *conversationCarry) copyRefusal(err error) string {
	return carryFailureReason(c.inAgentTerms(err))
}
