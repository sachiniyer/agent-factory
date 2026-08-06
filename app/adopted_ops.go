package app

import "github.com/sachiniyer/agent-factory/session"

// Provenance for in-flight ops on TUI rows (#3005).
//
// Two reconcile guards refuse to touch a row that has any op in flight: the
// same-title-different-session swap and the absent-from-snapshot removal. Both
// exist for LOCAL optimistic ops, and their comments say so — a create
// placeholder or an optimistic kill owns its own identity transition through its
// completion handler (instanceStartedMsg / instanceKilledMsg, by pointer
// identity, #808/#844), so the reconcile must not race it.
//
// adoptSnapshotOp then started mirroring DAEMON-owned ops onto the same field:
// OpArchiving, OpRestoring, OpReplacing, OpRespawning. Those have no local
// completion handler. Their only release is a later snapshot reporting OpNone
// for the SAME identity — so if the session is killed, or killed and recreated
// under the same title, before that snapshot arrives, the release never comes.
// Both guards then refuse forever and the row outlives its session: a corpse on
// the rail, attached to nothing, not removable, with its lifecycle controls
// suppressed. Restarting the TUI is the only recovery.
//
// The guard was asking "does something own this row's transition?" and answering
// "does this row have an op?" Those coincided until snapshot adoption landed
// (#1436).
//
// Provenance is RECORDED, never inferred. Inference by op KIND does not work in
// either direction: the TUI locally begins restores and archives, and the daemon
// projects OpCreating and OpKilling — so no op value is exclusively local or
// exclusively adopted. It is the same lesson as #2953, where four attempts tried
// to observe a fact from outside instead of recording it at the moment it was
// true.
//
// The record is a (pointer, op) PAIR, which is what makes it self-correcting: a
// local transition that changes the op leaves the recorded value no longer
// matching, so the row reads as locally owned again without every local call site
// having to remember to clear anything. Pointer-keyed for the same reason the
// completion handlers are — a same-title successor is a different row and must
// not inherit its predecessor's provenance.
type adoptedOps map[*session.Instance]session.InFlightOp

// note records that op on inst came from a daemon snapshot rather than a local
// optimistic transition.
func (a adoptedOps) note(inst *session.Instance, op session.InFlightOp) {
	if inst == nil {
		return
	}
	if op == session.OpNone {
		delete(a, inst)
		return
	}
	a[inst] = op
}

// owns reports whether inst's CURRENT op was adopted from a snapshot — i.e.
// whether no local completion handler is waiting on it.
//
// The op is compared, not merely the presence of an entry: if a local transition
// has since moved the row to a different op, that op is locally owned and the
// stale entry must not speak for it.
func (a adoptedOps) owns(inst *session.Instance) bool {
	if inst == nil {
		return false
	}
	op := inst.GetInFlightOp()
	if op == session.OpNone {
		return false
	}
	recorded, ok := a[inst]
	return ok && recorded == op
}

// forget drops a row's provenance. Called where a row leaves the store, so the
// map cannot outlive the instances it describes.
func (a adoptedOps) forget(inst *session.Instance) {
	delete(a, inst)
}

// pruneTo drops every entry whose instance is no longer among live.
//
// Chasing each lifecycle exit individually is what would rot: a row can leave the
// store through a project switch, a removal, a swap, or a local transition that
// clears the op without any snapshot involved, and each new exit added later
// would have to remember this map. Since the key is a pointer, a missed exit
// leaks the entry AND keeps the instance reachable, so the map would quietly
// become the thing preventing garbage collection of the rows it describes.
//
// Reconciling against the store instead makes the map self-correcting: whatever
// path a row left by, its entry is gone on the next snapshot.
func (a adoptedOps) pruneTo(live []*session.Instance) {
	if len(a) == 0 {
		return
	}
	keep := make(map[*session.Instance]struct{}, len(live))
	for _, inst := range live {
		keep[inst] = struct{}{}
	}
	for inst := range a {
		if _, ok := keep[inst]; !ok {
			delete(a, inst)
		}
	}
}

// vetoesReconcile reports whether inst's in-flight op should stop the reconcile
// from swapping or removing the row.
//
// This is the question both guards actually meant to ask. A LOCAL op vetoes:
// its completion handler owns the transition and the reconcile would orphan the
// handshake. An ADOPTED op does not: nothing local is waiting on it, and the
// snapshot is authoritative about whether the session still exists.
func (a adoptedOps) vetoesReconcile(inst *session.Instance) bool {
	if inst.GetInFlightOp() == session.OpNone {
		return false
	}
	return !a.owns(inst)
}

// adoptedOpsFor returns the model's provenance map, creating it on first write.
//
// Lazy rather than constructor-only because a `home` is built as a bare literal
// in several places (test helpers especially), and a nil map is fine to READ from
// but panics on assignment. Making the write path self-initializing means
// provenance cannot depend on which constructor a row arrived through — the exact
// kind of coupling that would make this correct in production and nil in a test.
func (m *home) adoptedOpsFor() adoptedOps {
	if m.adoptedSnapshotOps == nil {
		m.adoptedSnapshotOps = adoptedOps{}
	}
	return m.adoptedSnapshotOps
}
