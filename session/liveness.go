package session

import "fmt"

// Two-axis session state (#1195 Phase 1b).
//
// The legacy Status enum jammed two orthogonal axes into one int: what the
// backing session actually IS (liveness — daemon-owned, persisted) and what
// operation a client is mid-executing (an in-flight op — transient, never
// persisted). Overloading a single field is what generated the whole
// SetStatusIfNotDeleting / isTransientStatus / overrideDeleting apparatus that
// reconstructs the split at read time, and the #1187 collision (Deleting meaning
// both "TUI kill" and "daemon archive fence").
//
// This file introduces the two axes as separate fields and keeps the legacy
// Status enum working as a thin DERIVATION SHIM: GetStatus composes the old value
// from (liveness, inFlightOp) and SetStatus decomposes a legacy write back onto
// them. Phase 1b is deliberately INERT — no behavior changes — so later PRs can
// migrate writers (1c: daemon poll → SetLiveness) and readers (1d: reconcile →
// inFlightOp) onto the axes incrementally, then delete the shim (1e).

// Liveness is the daemon-owned health axis: what state the backing tmux/worktree
// is actually in, independent of any client operation in flight. It is the
// persisted half of the old Status enum — exactly the values SaveInstances ever
// wrote to disk (transients are skipped) — now named on its own axis.
type Liveness int

const (
	// LivenessUnset is the zero value. It is never a live in-memory state: it
	// exists so an InstanceData decoded from a record written before #1195 (no
	// `liveness` key) lands here and FromInstanceData falls back to the legacy
	// `status` int. omitempty drops it on write.
	LivenessUnset Liveness = iota
	// LiveRunning: the agent is working.
	LiveRunning
	// LiveReady: idle, waiting for user input.
	LiveReady
	// LiveLost: the backing session vanished under a live record — recovery-
	// eligible (#1108/#1104).
	LiveLost
	// LiveDead: legacy observed-death. Write-never since #1108 (deaths record
	// Lost); FromInstanceData maps persisted Dead→Lost. Retained so the shim
	// round-trips and a later PR can retire it (1e).
	LiveDead
	// LiveArchived: deliberately shelved, worktree moved out, inert (#1028).
	LiveArchived
	// LiveLimitReached: the agent hit a usage-limit wall (#1146). Folded in here
	// from the start so the limit epic consumes a Liveness value rather than
	// appending to the flat Status enum.
	LiveLimitReached
)

// InFlightOp is the client/executor-owned axis: the operation a client (or the
// daemon executor) is mid-way through, overlaid on the liveness. It is NEVER
// serialized and NEVER carried in the daemon snapshot — a transient overlay that
// clears when the liveness confirms the op's outcome. A separate field for it is
// what will retire the reconstruct-at-read apparatus (1d).
type InFlightOp int

const (
	// OpNone: no client operation in flight.
	OpNone InFlightOp = iota
	// OpCreating: a create is in flight (was Loading).
	OpCreating
	// OpKilling: an optimistic kill is in flight (was Deleting).
	OpKilling
	// OpArchiving: an archive teardown+move is in flight (was Deleting used as
	// the archive fence) — a distinct value from OpKilling so the two owners no
	// longer collide (#1187). Populated only through the shim in Phase 1b; the
	// daemon archive executor sets it directly in 1c.
	OpArchiving
	// OpRestoring: an archive restore is in flight (replaces the RestoreFromArchive
	// "park it in Lost to trigger the re-spawn loop" hack). Wired in 1c.
	OpRestoring
)

// composeStatus derives the legacy Status enum from the two-axis model. An
// in-flight op wins the composed value (it overlays the liveness), matching the
// old single-field semantics where Loading/Deleting masked the underlying state.
func composeStatus(lv Liveness, op InFlightOp) Status {
	switch op {
	case OpCreating:
		return Loading
	case OpKilling, OpArchiving:
		return Deleting
	case OpRestoring:
		return Lost
	}
	switch lv {
	case LiveRunning:
		return Running
	case LiveReady:
		return Ready
	case LiveLost:
		return Lost
	case LiveDead:
		return Dead
	case LiveArchived:
		return Archived
	case LiveLimitReached:
		// No legacy Status equivalent; nothing composes it until the limit
		// reader lands (1c/1d). Present the closest settled state meanwhile.
		return Ready
	}
	// LivenessUnset and any unknown value: preserve the old zero-value Status
	// (Running) so a bare Instance{} literal reads as it did pre-split.
	return Running
}

// LivenessForStatus maps a settled (non-transient) legacy Status to its Liveness
// axis. Transient values (Loading/Deleting) are handled by setStatusLocked, which
// sets the op and leaves liveness untouched, so they never reach here.
func LivenessForStatus(s Status) Liveness {
	switch s {
	case Running:
		return LiveRunning
	case Ready:
		return LiveReady
	case Lost:
		return LiveLost
	case Dead:
		return LiveDead
	case Archived:
		return LiveArchived
	}
	return LiveReady
}

// opForStatus extracts the in-flight-op axis from a legacy Status: only the
// transient values carry one, settled values map to OpNone. FromInstanceData
// uses it so rebuilding an instance from a snapshot that caught a transient
// (Loading/Deleting) reconstructs the same composed Status (the shim must be a
// faithful round-trip in Phase 1b).
func opForStatus(s Status) InFlightOp {
	switch s {
	case Loading:
		return OpCreating
	case Deleting:
		return OpKilling
	}
	return OpNone
}

// statusLocked composes the legacy Status under the caller-held mutex. It is the
// read half of the Phase 1b shim; every existing Status reader routes through it
// (GetStatus, ToInstanceData, the tab-spawn guards) unchanged.
func (i *Instance) statusLocked() Status {
	return composeStatus(i.liveness, i.inFlightOp)
}

// setStatusLocked decomposes a legacy Status write onto the two axes under the
// caller-held mutex. Transient values set the in-flight op and leave liveness
// untouched (they overlay it); settled values set liveness and clear the op —
// matching the old single-field clobber exactly.
func (i *Instance) setStatusLocked(s Status) {
	switch s {
	case Loading:
		i.inFlightOp = OpCreating
	case Deleting:
		i.inFlightOp = OpKilling
	default:
		i.inFlightOp = OpNone
		i.liveness = LivenessForStatus(s)
	}
}

// SetStatus sets the status under the instance mutex (legacy shim — decomposes
// onto the two axes).
func (i *Instance) SetStatus(status Status) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setStatusLocked(status)
}

// GetStatus returns the current status under the Instance's mutex, so
// cross-goroutine readers don't race with SetStatus (legacy shim — composes the
// value from the two axes).
func (i *Instance) GetStatus() Status {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.statusLocked()
}

// GetLiveness returns the daemon-owned health axis under the instance mutex.
func (i *Instance) GetLiveness() Liveness {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness
}

// SetLiveness writes the health axis under the instance mutex, leaving the
// in-flight op untouched. This is what the daemon poll uses instead of the old
// SetStatusIfNotDeleting: writing liveness can never clobber an in-flight op
// because the op is a separate field, so no "if not deleting" guard is needed —
// a concurrent kill/archive op survives the write and still composes to the
// transient status (#1195).
func (i *Instance) SetLiveness(lv Liveness) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.liveness = lv
}

// GetInFlightOp returns the client/executor op axis under the instance mutex.
func (i *Instance) GetInFlightOp() InFlightOp {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.inFlightOp
}

// IsCreating reports whether a create is in flight (the render/gate replacement
// for the old GetStatus()==Loading check).
func (i *Instance) IsCreating() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.inFlightOp == OpCreating
}

// IsTearingDown reports whether a kill or archive teardown is in flight (the
// render/gate replacement for the old GetStatus()==Deleting check — both owners
// now live on distinct ops, but both read as "going away").
func (i *Instance) IsTearingDown() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.inFlightOp == OpKilling || i.inFlightOp == OpArchiving
}

// HasInFlightOp reports whether any client op is in flight (the render/gate
// replacement for the old Loading||Deleting "is this row transient" check).
func (i *Instance) HasInFlightOp() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.inFlightOp != OpNone
}

// SetInFlightOp writes the op axis under the instance mutex, leaving the liveness
// untouched. The daemon archive executor uses it to raise OpArchiving as a fence
// over its teardown+move window (and to clear it back to OpNone on a rollback).
func (i *Instance) SetInFlightOp(op InFlightOp) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.inFlightOp = op
}

// MarkLive flips the instance to Running and clears a completing create/restore
// op — the two-axis translation of the old SetStatusIfNotDeleting(Running) that
// backend Start/Recover used. It preserves a teardown fence (OpKilling/
// OpArchiving): a Start/Recover completing must never resurrect a session that a
// kill or archive is tearing down; that owner writes the terminal liveness. It
// does clear OpCreating/OpRestoring, which is exactly the op this completion
// resolves.
func (i *Instance) MarkLive() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp == OpKilling || i.inFlightOp == OpArchiving {
		return
	}
	i.liveness = LiveRunning
	i.inFlightOp = OpNone
}

// tabSpawnBlockedLocked reports the error, if any, forbidding a new tab spawn.
// Caller holds i.mu. It reads the two axes directly (the #1195 structural fold of
// the #1196 archive-orphan guard): a tab may not spawn into an archived session
// (its worktree was moved away) or one with a teardown op in flight — an archive
// (OpArchiving) or kill (OpKilling). The archive case is the load-bearing one:
// ArchiveTeardown keeps started=true, so the #990 started-flag guard never fires
// during archive; OpArchiving is the fence that started=true cannot provide.
func (i *Instance) tabSpawnBlockedLocked() error {
	if i.liveness == LiveArchived {
		return fmt.Errorf("cannot add a tab to an archived session; restore it first (af sessions restore)")
	}
	if i.inFlightOp == OpArchiving || i.inFlightOp == OpKilling {
		return fmt.Errorf("cannot add a tab to a session that is being archived or removed; try again in a moment")
	}
	return nil
}

// TabSpawnBlocked is the locking form of tabSpawnBlockedLocked, for callers that
// don't already hold i.mu (the daemon's archive-exclusive tab lock).
func (i *Instance) TabSpawnBlocked() error {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.tabSpawnBlockedLocked()
}
