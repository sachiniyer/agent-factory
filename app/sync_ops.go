package app

import (
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The in-flight-operation axis of the snapshot reconcile (reconcileSnapshotOp,
// adoptSnapshotOp) and the liveness resolution it settles against
// (snapshotLiveness). Split out of sync.go (#1145 file-length limit): that file
// reached 1002 lines when several independently green PRs merged together, and
// this block is a self-contained unit with no state of its own. Its callers,
// reconcileSnapshot and updateInstanceFromSnapshot, stay in sync.go.

// reconcileSnapshotOp mirrors the daemon snapshot's operation axis while
// preserving local optimistic operations until the daemon liveness confirms an
// outcome. Snapshot ops are transient but authoritative for daemon-side
// archive/restore windows (#1436); terminal snapshots carry OpNone and clear
// stale overlays through the Transition chokepoint.
func (m *home) reconcileSnapshotOp(inst *session.Instance, op session.InFlightOp, lv session.Liveness) bool {
	if op != session.OpNone {
		return m.adoptSnapshotOp(inst, op, lv)
	}

	switch inst.GetInFlightOp() {
	case session.OpArchiving, session.OpKilling:
		if lv == session.LiveArchived || lv == session.LiveLost {
			_ = inst.Transition(session.ClearOp())
			m.adoptedSnapshotOps.forget(inst)
			return true
		}
		// CancelArchive (daemon/archive.go) reverts an in-flight archive to
		// OpNone while PRESERVING the prior non-terminal liveness — a
		// third outcome the terminal-liveness-only gate above did not model.
		// Without this arm, an ADOPTED OpArchiving stays stranded on the row
		// across every later poll: the snapshot reports OpNone, this branch
		// is hit, and the non-terminal liveness skips the body. Only a
		// daemon-owned op is released here — a LOCAL optimistic
		// OpArchiving/OpKilling has its own completion handler that owns the
		// clear (instanceArchivedMsg / instanceKilledMsg), and owns(inst)
		// returns false for it, preserving the kill UX guarded by
		// TestReconcile_OptimisticKillPreservedForNonTerminal.
		if m.adoptedSnapshotOps.owns(inst) {
			_ = inst.Transition(session.ClearOp())
			m.adoptedSnapshotOps.forget(inst)
			return true
		}
	case session.OpRestoring:
		if lv != session.LiveArchived {
			_ = inst.Transition(session.ClearOp())
			m.adoptedSnapshotOps.forget(inst)
			return true
		}
	case session.OpReplacing:
		// A daemon handoff owns the replacement fence. Once its snapshot settles
		// to OpNone, the projection must release the same fence regardless of the
		// resulting liveness (running, limit-parked, or startup-unknown).
		_ = inst.Transition(session.ClearOp())
		m.adoptedSnapshotOps.forget(inst)
		return true
	case session.OpRespawning:
		// Same contract for the limit-resume fence (#2997), and for the same reason:
		// a respawn settles to running, back to limit-parked, or to lost, so keying
		// the release on any particular liveness would strand the fence on the
		// others. This clause is what makes adopting the op above safe — and unlike
		// OpCreating, a missed release would not even show, since OpRespawning
		// composes to the settled liveness rather than masking it as Loading.
		_ = inst.Transition(session.ClearOp())
		// Drop the provenance with the op, exactly as the three cases above do
		// (#3005/#3006 landed after this case was written). Leaving it is benign only
		// by accident — vetoesReconcile short-circuits on OpNone, so the stale entry is
		// never read — but it survives pruneTo for as long as the row is live, and the
		// next path that consults provenance without checking the op first would
		// inherit a wrong answer.
		m.adoptedSnapshotOps.forget(inst)
		return true
	}
	return false
}

func (m *home) adoptSnapshotOp(inst *session.Instance, op session.InFlightOp, lv session.Liveness) bool {
	if inst.GetInFlightOp() == op {
		// Already carrying it, but provenance may not be recorded yet — a row whose
		// op the daemon reported before this build learned to track it, or one this
		// reconcile adopted on an earlier pass. Record it either way: the guards ask
		// about the CURRENT op, not about who set it first.
		m.adoptedOpsFor().note(inst, op)
		return false
	}
	if inst.GetInFlightOp() != session.OpNone {
		_ = inst.Transition(session.ClearOp())
	}

	var err error
	switch op {
	case session.OpCreating:
		err = inst.Transition(session.BeginCreate())
	case session.OpKilling:
		err = inst.Transition(session.BeginKill())
	case session.OpArchiving:
		if lv == session.LiveArchived {
			m.adoptedOpsFor().note(inst, op)
			return true
		}
		err = inst.Transition(session.BeginArchive())
	case session.OpRestoring:
		err = inst.Transition(session.MarkRestoring())
	case session.OpReplacing:
		err = inst.Transition(session.BeginHandoff())
	case session.OpRespawning:
		// A limit resume re-spawning the runtime (#2997). Adopted so this TUI's action
		// gating agrees with the daemon that owns the fence. It DOES also change how
		// the row reads, which an earlier version of this comment wrongly denied:
		// ui/tree/render.go masks the liveness glyph for any non-None op, so a
		// respawning row shows the blank working glyph rather than its limit diamond,
		// and web/src/status.ts classifies it as working (project.ts counts it that
		// way). That is consistent with every other op and with #1766 — a resume in
		// flight IS work in flight — and the "[limit] resets …" title prefix still
		// renders throughout, so the state is not hidden. Without adopting, this TUI's own
		// ValidateRuntimeAction(RuntimeActionHandoff) reads OpNone and opens the
		// picker, and the daemon then refuses the selected handoff because its
		// authoritative instance still carries the fence — an action offered and
		// then rejected. The settle path below must clear it; see there for why
		// adopting is safe here in a way adopting OpCreating on an established row
		// was not.
		err = inst.Transition(session.BeginRespawn())
	default:
		return false
	}
	if err != nil {
		log.WarningLog.Printf("failed to adopt snapshot op %v for %q: %v", op, inst.Title, err)
		return false
	}
	// The row now carries a DAEMON-owned op with no local completion handler
	// waiting on it. Recorded here, at the only moment the fact is known (#3005).
	m.adoptedOpsFor().note(inst, op)
	return true
}

// snapshotLiveness resolves the daemon-owned liveness a snapshot record carries
// (#1195). It prefers the explicit `liveness` field; a record from a daemon that
// predates the field (LivenessUnset) falls back to the legacy `status` int.
//
// A legacy TRANSIENT (Loading/Deleting) carries NO real liveness in the two-axis
// model — the new daemon only ever sends liveness, so a legacy transient is
// version-skew noise from an upgrade window. It must NOT be mapped to Ready
// (LivenessForStatus's default), which would flip a mid-kill row (legacy
// `status: Deleting`) to Ready during the window. Instead we keep the row's
// CURRENT liveness untouched; the daemon reports a real liveness on a later poll
// once it's upgraded. `cur` is the existing row's liveness for exactly this.
func snapshotLiveness(cur session.Liveness, d session.InstanceData) session.Liveness {
	if d.Liveness != session.LivenessUnset {
		return d.Liveness
	}
	if d.Status == session.Loading || d.Status == session.Deleting {
		return cur
	}
	return session.LivenessForStatus(d.Status)
}
