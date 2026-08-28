package session

import "time"

// Orphan capture for a create that provisioned a sandbox it then could not use
// (#3480).
//
// #3475/#3478 fixed the RESTORE side of this: reprovisionRemote's post-provision
// failure exits retain the cleanup handle on the Instance when a teardown comes
// back unconfirmed, because that handle is the last pointer to a sandbox that may
// still be running. The CREATE side has the identical condition and could not use
// the identical answer — it fails BEFORE an Instance exists, so there was nowhere
// to install a handle, and the possible orphan was recorded only as text in a
// returned error. A container or remote workspace kept billing with nothing on
// af's side able to name it.
//
// The daemon already owns the whole mechanism for this, built for a different
// failure: keepFailedCreate tombstones a create whose cleanup could not complete,
// SaveInstances keeps a tombstoned row through wholesale checkpoints,
// refreshInstanceStatus routes it to finishUserKill on every poll, and
// CleanupRetry paces that retry and retires a handle that cannot ever succeed.
// All of it is Instance-shaped, and all of it was unreachable from a create
// failure for one reason: NewInstance returns nil.
//
// So this adds no retry loop, no persistence format and no new durable state. It
// carries the sandbox's identity out of the failure through the one channel that
// already crosses it — the error — as a cleanup-only record the daemon hands
// straight to the keepFailedCreate it already calls for a failed Start.

// SandboxOrphanError reports a create that failed leaving behind a sandbox whose
// teardown could not be confirmed.
//
// It WRAPS rather than replaces the cause, so every caller that does not know
// about it keeps seeing exactly the error it sees today, sentinels included. That
// is what makes this additive for the two create callers which cannot persist
// anything — the TUI's inert naming placeholder and the in-sandbox agent-server,
// both pinned to the local runtime, which provisions no sandbox and hands back no
// teardown, so neither can ever produce one of these.
type SandboxOrphanError struct {
	record *Instance
	cause  error
}

func (e *SandboxOrphanError) Error() string { return e.cause.Error() }

// Unwrap keeps the cause's chain reachable, so TeardownStateUnknown and every
// errors.Is on the original failure still answer the same as before.
func (e *SandboxOrphanError) Unwrap() error { return e.cause }

// OrphanRecord is a cleanup-only session record for the sandbox that may still be
// running: the provisioned backend's durable teardown identity, its live handle,
// and the markers that make both survive to the next daemon.
//
// It is NOT a usable session — no agent, no worktree, no client — and it is born
// TOMBSTONED (see orphanIfUnconfirmed) so a caller that persists it without
// reading this comment still gets "finish this teardown, never restore it".
func (e *SandboxOrphanError) OrphanRecord() *Instance { return e.record }

// orphanIfUnconfirmed wraps err in a *SandboxOrphanError when it reports a
// sandbox this create could not confirm torn down.
//
// THE one thing the two create-path failure exits share, and therefore the one
// place it lives. What they do NOT share is how a KNOWN teardown error is
// reported: NewInstance surfaces it, because a create failing for a reason it can
// name should say so, while discardUnusableSandbox logs it and returns the cause
// alone. That difference is a decision rather than drift, so this keys off the
// classification each exit already performed instead of redoing it — a confirmed
// teardown returns err untouched, and only "it may still be running" produces a
// record.
func orphanIfUnconfirmed(title, path string, res ProvisionResult, err error) error {
	if err == nil || !TeardownStateUnknown(err) {
		return err
	}
	if res.Backend == nil {
		// Nothing to name, so nothing a row could ever reap. The error already says
		// a sandbox may still be running; manufacturing a handle-less record would
		// only hold a title hostage to something no retry can reach.
		return err
	}
	now := time.Now()
	record := &Instance{
		ID:        NewInstanceID(),
		Title:     title,
		Path:      path,
		CreatedAt: now,
		UpdatedAt: now,
		// Lost, not Ready: no agent ever ran on this sandbox. Retention comes from
		// the unknown-cleanup marker and the tombstone below, never from a row that
		// looks alive.
		liveness: LiveLost,
	}
	// The same installer the restore path uses, for the same reason — identity plus
	// cleanup handle, and deliberately no client, so AgentServer() builds a
	// deadRemoteAgentServer whose Kill retries this exact cleanup.
	record.retainProvisionResultCleanup(res)
	// Born a tombstone. keepFailedCreate marks this itself and is idempotent, so
	// this is not needed to reach the daemon's path; it is here because the
	// alternative default is dangerous. A sandbox-backed row that is Lost and NOT
	// tombstoned is exactly the shape the Lost-restore loop picks up, so a record
	// that escaped without one could be "recovered" into a fresh sandbox beside the
	// orphan — the #3475 harm, re-entered from the create side.
	record.MarkUserKilled()
	return &SandboxOrphanError{record: record, cause: err}
}
