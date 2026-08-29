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

// sandboxIdentity is who a provisioned sandbox belongs to: enough to build a
// cleanup-only record for it, and deliberately nothing more.
//
// The id matters as much as the title. A daemon create publishes a provisional
// row under pending.ID and passes that same id into NewInstance so every later
// projection UPSERTS onto it; a retained row that minted its own id would reach
// clients as a second identity beside the pending one instead of replacing it.
// createdAt is carried for the same reason as id: the daemon publishes the
// provisional row with one, and a retained row that stamped time.Now() instead
// would jump in the clients' rail order at the moment it settles.
type sandboxIdentity struct {
	id, title, path string
	createdAt       time.Time
}

// newSandboxOrphanError builds the orphan error for a sandbox whose teardown
// could not be confirmed.
//
// It does NOT re-derive that classification from the error chain, and that is the
// whole point of the signature: it is reached only from a branch that already
// established the teardown came back unknown. An earlier version of this tested
// TeardownStateUnknown on the COMBINED error instead, which is a different
// question — the revalidation cause can carry a teardown sentinel of its own, and
// that manufactured an orphan record, a tombstone and a held title for a sandbox
// the reap had already confirmed gone.
func newSandboxOrphanError(who sandboxIdentity, res ProvisionResult, cause error) error {
	if res.Backend == nil {
		// Nothing to name, so nothing a row could ever reap. The error already says
		// a sandbox may still be running; manufacturing a handle-less record would
		// only hold a title hostage to something no retry can reach.
		return cause
	}
	id := who.id
	if id == "" {
		id = NewInstanceID()
	}
	created := who.createdAt
	if created.IsZero() {
		created = time.Now()
	}
	record := &Instance{
		ID:        id,
		Title:     who.title,
		Path:      who.path,
		CreatedAt: created,
		UpdatedAt: time.Now(),
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
	return &SandboxOrphanError{record: record, cause: cause}
}
