package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/session/git"
)

// MissionBrief is what an incoming agent is told when a session is handed to it
// (#2013, design decision D2). It is the entire state transfer: the goal, the
// branch, and what is already on it.
//
// Deliberately absent: any summary of what the previous agent was thinking, or
// of what its diff means. af did not do that work and cannot describe it
// truthfully — any prose it invented would be an inference presented to the new
// agent as established fact, which is exactly the blended-context hazard #2013
// asks us to avoid. The diff is the ground truth and the incoming agent can
// read it, so the brief points at it instead of paraphrasing it.
type MissionBrief struct {
	// Goal is the mission: the session's stored prompt, or an operator-supplied
	// override. Empty when the session never carried one.
	Goal string
	// From and To are the outgoing and incoming agent names.
	From string
	To   string
	// Reason is why the handoff happened, rendered into the brief so the new
	// agent knows its predecessor stopped for an external reason and did not
	// simply fail.
	Reason string
	// Work is the branch state at handoff time.
	Work git.WorkSummary
}

// BuildMissionBrief assembles the brief for handing this instance to `to`.
//
// override wins over the stored prompt when non-empty: it is what a user typed
// at the moment of the handoff, so it is both more specific and more current
// than a prompt stored at create time. When neither exists the brief simply has
// no goal line — see Render, which says so out loud rather than fabricating one.
//
// Best-effort on the git side: a worktree that cannot be summarized still yields
// a brief, minus the "work already done" section.
func (i *Instance) BuildMissionBrief(to, override, reason string) MissionBrief {
	brief := MissionBrief{
		From:   i.CurrentAgentName(),
		To:     strings.TrimSpace(to),
		Reason: strings.TrimSpace(reason),
	}

	if goal := strings.TrimSpace(override); goal != "" {
		brief.Goal = goal
	} else {
		brief.Goal = strings.TrimSpace(i.Prompt)
	}

	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw != nil {
		if summary, err := gw.WorkSummary(); err == nil {
			brief.Work = summary
		} else {
			// Keep the branch name even when counting failed — it is the one fact
			// the new agent needs to look around for itself.
			brief.Work = git.WorkSummary{Branch: gw.GetBranchName()}
		}
	}

	return brief
}

// Render produces the prompt delivered to the incoming agent.
//
// Every clause is something af actually knows. Where it knows nothing — no
// stored goal, no commits — it says that plainly, because a brief that invents
// a goal is worse than one that admits it has none: the agent would pursue the
// invention.
func (m MissionBrief) Render() string {
	var b strings.Builder

	from := m.From
	if strings.TrimSpace(from) == "" {
		from = "another agent"
	}
	b.WriteString("You are continuing work that is already in progress in this worktree.\n\n")
	fmt.Fprintf(&b, "It was being done by %s, which %s. "+
		"Its conversation is not available to you — only the working tree and its git history are.\n",
		from, stoppedClause(m.Reason))

	if m.Goal != "" {
		b.WriteString("\nThe original goal:\n\n")
		b.WriteString(m.Goal)
		b.WriteString("\n")
	} else {
		b.WriteString("\nNo goal was recorded for this session, so infer the intent from the work already " +
			"on the branch before changing anything.\n")
	}

	if branch := strings.TrimSpace(m.Work.Branch); branch != "" {
		fmt.Fprintf(&b, "\nWork already done on branch %s:\n", branch)
		switch {
		case m.Work.Empty():
			b.WriteString("  Nothing yet — no commits on top of the base, and the working tree is clean.\n")
		default:
			fmt.Fprintf(&b, "  %s, %s.\n", pluralize(m.Work.Commits, "commit"), pluralize(m.Work.DirtyFiles, "uncommitted file"))
			if base := strings.TrimSpace(m.Work.BaseSHA); base != "" {
				fmt.Fprintf(&b, "  Review it before you start:  git log %s..HEAD  ·  git diff %s...HEAD\n", base, base)
			} else {
				b.WriteString("  Review it before you start:  git log  ·  git status\n")
			}
		}
	}

	b.WriteString("\nContinue from that state. Do not start over, and do not revert work you did not write.\n")

	return b.String()
}

// stoppedClause turns a handoff reason into the verb clause the brief reads
// with. The HandoffReason* constants are LABELS — they name a reason in a
// ledger or a status line — and interpolating one into a sentence produced
// "which stopped because it hit manual", which is not English and is the first
// thing the incoming agent reads.
//
// The mapping is explicit rather than a format string per reason so that a
// reason with no clause degrades to a true, if vaguer, sentence instead of a
// broken one. That matters more here than in most copy: the brief IS the state
// transfer, and an agent that opens on a garbled sentence has been told, in the
// same breath, that the record it is inheriting is unreliable.
func stoppedClause(reason string) string {
	switch strings.TrimSpace(reason) {
	case HandoffReasonUsageLimit:
		return "stopped because it hit its usage limit"
	case HandoffReasonManual:
		return "was handed over to you before the work was finished"
	default:
		return "stopped before the work was finished"
	}
}

func pluralize(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
