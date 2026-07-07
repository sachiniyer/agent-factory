package session

import (
	"fmt"
	"time"
)

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

// GetLiveness returns the daemon-owned liveness axis under the Instance's mutex
// (#1146/#1195). Readers use it where the composed Status is lossy: the snapshot
// reconcile mirrors liveness (not Status) so LiveLimitReached — which composes to
// Ready — propagates from the daemon to the read-only TUI.
func (i *Instance) GetLiveness() Liveness {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness
}

// SetLiveness is retired (#1195 Phase 2e): the liveness axis is now written only
// through the Transition chokepoint's ObserveLiveness edge (the daemon-truth
// edge — sets liveness, preserves the in-flight op, never rejects), so there is
// no direct liveness setter left to bypass it.

// GetInFlightOp returns the client/executor op axis under the instance mutex.
func (i *Instance) GetInFlightOp() InFlightOp {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.inFlightOp
}

// ShownArchived reports whether the row belongs in the sidebar's Archived
// section (#1028): it is archived on the liveness axis AND not mid-restore. An
// OpRestoring overlay re-homes the row into the live Instances section EAGERLY
// (#1210) — the visible feedback the archive epic owes restore — WITHOUT touching
// the liveness axis. Leaving liveness LiveArchived is load-bearing: the snapshot
// reconcile keys its Archived→live REBUILD (re-Start, restoring started + the
// agent-tmux binding, #1203) on seeing that exact transition, so an eager
// liveness flip here would make the reconcile see live→live and SKIP the rebuild,
// stranding the restored row "live but not started" — the #1203 regression. The
// rebuild replaces the row with a fresh started instance (OpNone), which clears
// the overlay; a restore FAILURE clears OpRestoring so the row drops back into
// the Archived section.
func (i *Instance) ShownArchived() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness == LiveArchived && i.inFlightOp != OpRestoring
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

// SetInFlightOpForTest writes the op axis directly, for TEST scaffolding only —
// establishing a precondition state rather than exercising a transition (#1195
// Phase 2e). Production code never sets the op axis directly: every op write goes
// through the Transition chokepoint (BeginCreate/BeginKill/BeginArchive/
// BeginRestore/MarkRestoring raise an op; ConfirmLive/RevertKill/CommitArchive/
// AbortArchive/AbortRestore/ClearOp clear it). Mirrors the SetStartedForTest /
// SetGitWorktreeForTest scaffolding pattern.
func (i *Instance) SetInFlightOpForTest(op InFlightOp) {
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

// SetLimitReached marks the instance blocked on a usage-limit wall (#1146): it
// sets the LiveLimitReached liveness and stores the parsed reset time (zero when
// the banner carried none) for the sidebar badge and PR3's auto-resume
// scheduler. There is no legacy Status value for SetStatus to decompose onto, so
// the daemon single-writer (#960) sets the liveness axis directly here. Skips a
// row mid create/kill teardown so it never clobbers an in-flight op, mirroring
// SetStatusIfNotDeleting.
func (i *Instance) SetLimitReached(resetAt time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if s := i.statusLocked(); s == Loading || s == Deleting {
		return
	}
	i.inFlightOp = OpNone
	i.liveness = LiveLimitReached
	i.limitResetAt = resetAt
}

// SetLimitResetAt records only the display-only reset time (#1146), leaving both
// axes untouched. The read-only TUI reconcile uses it to mirror the daemon's
// parsed reset time after it has already applied LiveLimitReached on the liveness
// axis (Phase 1d applies liveness UNCONDITIONALLY via SetLiveness): the reset
// time rides the liveness as pure display metadata, so it is set on its own
// rather than through SetLimitReached, which would re-drive the liveness axis and
// carry SetLimitReached's transient-op guard into a path that must be
// unconditional.
func (i *Instance) SetLimitResetAt(resetAt time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.limitResetAt = resetAt
}

// ClearLimitReached moves a limit-blocked instance back to LiveRunning so the
// daemon poll re-resolves its real state on the next tick and the [limit] badge
// clears (#1146). A no-op when the instance is not limit-blocked, so the resume
// action (and PR3's scheduler) can call it unconditionally.
func (i *Instance) ClearLimitReached() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.liveness != LiveLimitReached {
		return
	}
	i.liveness = LiveRunning
	i.limitResetAt = time.Time{}
}

// LimitReached reports whether the instance is blocked on a usage limit (#1146).
// Every render/serialize site keys its [limit] badge off this rather than the
// composed Status, which has no limit value (LiveLimitReached composes to Ready).
func (i *Instance) LimitReached() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness == LiveLimitReached
}

// LimitResetAt returns the parsed usage-limit reset time and whether one is
// known (#1146). It reports (zero, false) when the session is not limit-blocked
// or the banner carried no parseable reset time, so a stale reset value can
// never leak onto a recovered session.
func (i *Instance) LimitResetAt() (time.Time, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.liveness != LiveLimitReached || i.limitResetAt.IsZero() {
		return time.Time{}, false
	}
	return i.limitResetAt, true
}

// livenessFromData resolves the liveness a persisted or snapshot record should
// take, applying the same rollforward FromInstanceData uses: prefer the
// `liveness` field, fall back to the legacy `status` int for pre-#1195 records,
// and map a persisted Dead → Lost (recovery-eligible, #1108). Shared so the
// snapshot reconcile mirrors liveness identically to a cold-start restore.
func livenessFromData(data InstanceData) Liveness {
	lv := data.Liveness
	if lv == LivenessUnset {
		lv = LivenessForStatus(data.Status)
	}
	if lv == LiveDead {
		lv = LiveLost
	}
	return lv
}
