package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// resolveIdleLiveness settles a session whose pane DID NOT CHANGE this tick
// (#1146): it sets LimitReached when the captured content shows a usage-limit
// banner for the resolved agent (claude/codex/devin have matchers), else Running when
// the agent is visibly still mid-turn, else the plain Ready liveness. A limit
// session must never render Ready, which is why the detector runs before the
// Ready fallback. Self-recovery (the banner scrolls away) or resumed work (the
// `updated` branch → Running) clears the limit liveness on its own later tick.
// What the transition does NOT clear is the durable per-account observation the
// wall recorded — that is refuted here and in the `updated` branch, on the same
// affirmative-work evidence (#4404). Split from refreshInstanceStatus so
// control.go stays under its length ceiling (#1145).
//
// "Did not change" is NOT the same as "is idle", which is the whole reason the
// working check sits here. The caller reaches this branch on pane STILLNESS, and
// stillness is only evidence of idleness for agents that repaint while they work
// (claude/codex animate a spinner + elapsed timer; amp does not — it holds a
// static frame through every quiet gap in a turn). So before settling Ready, ask
// the agent: a pane that says it is working IS working, and stays Running. See
// task.IsWorkingContent for why a debounce cannot stand in for this.
//
// epoch is the instance's state epoch captured BEFORE the pane capture `content`
// came from, and every write below is scoped to it (#2135). This whole function
// is a conclusion about a session as it was when its pane was read, and between
// that read and here an authoritative transition can land — above all a resume
// (the manual `c` retry or the auto-resume scheduler) clearing the usage-limit
// block, re-delivering the prompt and persisting LiveRunning. Applying the
// detector's hit on top of that re-parked a session that was in fact working, and
// the persist gate then wrote the reverted state to disk. So the applies here
// carry the epoch: the state moved ⇒ this decision is about a state the session
// has already left, and it is dropped rather than clobbering the newer one. The
// next tick re-decides from content captured after the transition, which is why
// nothing is lost — see session/state_epoch.go.
//
// The return reports whether affirmative work evidence refuted durable
// account-limit evidence for the session's identity (#4404) — a durable
// one-shot change the caller folds into its settlement checkpoint.
func (m *Manager) resolveIdleLiveness(instance *session.Instance, content string, epoch uint64) bool {
	agent := instance.ResolvedAgent()
	if hit, resetAt, _ := m.limitDetector.Load().Check(content, agent, time.Now()); hit {
		// Returns false when the decision was superseded; nothing to do about it
		// here — the next tick observes the session as it is now.
		_ = m.setLimitReachedAtEpoch(instance, resetAt, epoch)
		return false
	}
	if task.IsWorkingContent(content, agent) {
		// Still mid-turn behind a still pane: hold Running so the #1766 status dot
		// stays dark. Settling Ready here is the green flash (#1766 says green ==
		// waiting for you), and it is not merely cosmetic — a Ready amp is what
		// `af sessions watch` unblocks on and what tells a user their turn is done.
		//
		// The refute runs BEFORE the transition: a limit-parked session whose
		// pane kept working is exactly the stale-evidence case, and the
		// transition's own epoch bump would drop a refutation applied after it.
		refuted := m.refuteAccountLimitEvidence(instance, epoch)
		_ = instance.Transition(session.ObserveLiveness(session.LiveRunning).AtEpoch(epoch))
		return refuted
	}
	// Plain idle: settle to Ready. On the two-axis model (#1195) SetLiveness
	// writes only the liveness axis and never clobbers an in-flight op, so it
	// needs no "if not deleting" guard — this is exactly what the poll's Ready
	// fallback does inline in refreshInstanceStatus.
	_ = instance.Transition(session.ObserveLiveness(session.LiveReady).AtEpoch(epoch))
	return false
}

// updatedPaneAccountVerdict applies the wall-or-refute verdict above to a pane
// that CHANGED this tick — the changed-content sibling of the still-pane check.
// Fresh bytes are affirmative-work evidence ONLY when they are not themselves a
// usage-limit banner: a wall repainting (its reset countdown ticking, the
// banner scrolling into view) produces updated content too, and refuting on it
// retracts the very evidence the banner is proving — then routes the next
// create onto an account that is still walled (#4404 review). A detected banner
// is therefore parked at the wall it shows, exactly as a still pane would be.
//
// What counts as refuting work differs by agent. Agents WITH a working-content
// indicator (amp, opencode) refute only on task.IsWorkingContent — churn that
// is neither banner nor working content proves only that the pane is alive,
// not that the account answered. The account-scoped agents — claude, codex,
// gemini — have no such indicator BY DESIGN: pane churn is the whole of their
// work signal, so gating their refutation on IsWorkingContent made it
// unreachable for exactly the identities account-limit evidence exists for
// (#4404 review). For them fresh non-banner output IS the refutation: a
// session parked at its wall emits nothing but the banner's own repaint.
//
// Returns (walled, refuted): walled tells the caller NOT to transition the
// session Running — the pane just proved the opposite — and refuted feeds the
// caller's settlement checkpoint fold like the resolveIdleLiveness return does.
func (m *Manager) updatedPaneAccountVerdict(instance *session.Instance, content string, epoch uint64) (walled, refuted bool) {
	agent := instance.ResolvedAgent()
	if hit, resetAt, _ := m.limitDetector.Load().Check(content, agent, time.Now()); hit {
		// The wall could not have been re-parked on the still-pane path — the
		// pane CHANGED — so this is the tick that must record it. A superseded
		// epoch answer is harmless exactly as it is there.
		_ = m.setLimitReachedAtEpoch(instance, resetAt, epoch)
		return true, false
	}
	if task.IsWorkingContent(content, agent) {
		return false, m.refuteAccountLimitEvidence(instance, epoch)
	}
	if _, accountScoped := sessionenv.SupportsAccounts(agent); accountScoped {
		return false, m.refuteAccountLimitEvidence(instance, epoch)
	}
	return false, false
}
