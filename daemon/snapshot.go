package daemon

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// Snapshot RPC types, the delivery-failure alarm projection, and the Snapshot
// handler itself (#960 PR 3, #1238). Extracted from control.go so that file stays
// under its length ceiling (#1145).
//
// The HANDLER moved here from control_server.go in #3056, when the owner
// constraint pushed that file back over the limit — so this file now owns the
// wire shapes, the alarm assembly, and the read that produces both. That is the
// right grouping anyway: the constraint below decides what goes into these types,
// and splitting the two across files is how they drift.
//
// The TUI's SnapshotWithAlarms read moved onto the HTTP apiclient in #1592 Phase
// 2 PR3 (apiclient.Client.SnapshotWithAlarms), so the net/rpc SnapshotWithAlarms
// client wrapper that used to live here is gone — the TUI was its only caller.
// The sessions SnapshotNoSpawn read moved to apiclient in Phase 2 PR2; task
// reads (ListTasksNoSpawn) stay on net/rpc.

// SnapshotRequest asks the daemon for the authoritative session list of a repo
// (#960 PR 3). RepoID scopes the read like the other sessions verbs (empty =
// all repos). It is the read side of the single-writer model: the daemon's
// in-memory instance map is the source of truth, so the TUI mirrors this
// projection instead of re-reading instances.json off disk.
type SnapshotRequest struct {
	RepoID string `json:"repo_id"`
	// Live excludes deliberately archived rows. It is distinct from Statuses so
	// callers can ask for every non-archived lifecycle state without copying the
	// daemon's status vocabulary into their scripts.
	Live bool `json:"live,omitempty"`
	// Statuses is an OR filter over canonical liveness names. All other filters
	// compose with it as AND constraints.
	Statuses []string `json:"statuses,omitempty"`
	// CreatedAfter excludes sessions created before this absolute bound. The CLI's
	// --max-age flag resolves to this timestamp once, so daemon and disk-fallback
	// reads cannot disagree at a moving duration boundary.
	CreatedAfter time.Time `json:"created_after,omitzero"`
	// Limit bounds the filtered result. A pointer distinguishes an omitted,
	// backward-compatible unbounded request from an explicit invalid zero.
	Limit *int `json:"limit,omitempty"`

	// The remaining fields are the lazy-archive shape (#6178). They are additive:
	// a request that sets none of them is answered exactly as before, and a daemon
	// that predates them ignores them and answers with every row, so a client
	// that asked for LazyArchive detects the absence of ArchivedTotal in the reply
	// and treats the rows it got as the complete list.

	// LazyArchive asks for the live (non-archived) rows only, with ArchivedTotal
	// and ArchivedByProject standing in for the archived rows left out — counted
	// over the repo-scoped rows BEFORE any other filter. It is one switch, not
	// Live plus a summary flag, so a daemon that honors Live but predates the
	// summary cannot drop the archived rows without saying how many there are.
	LazyArchive bool `json:"lazy_archive,omitempty"`
	// NewestFirst orders the filtered rows newest-created first, ties broken by
	// descending id, so a page boundary is a stable (created_at, id) position.
	NewestFirst bool `json:"newest_first,omitempty"`
	// Before resumes a NewestFirst listing after the row a previous reply's
	// NextBefore named: only rows strictly older in that order are returned.
	Before *SnapshotCursor `json:"before,omitempty"`
	// ProjectPath keeps only rows whose worktree repo path is exactly this. It is
	// the key the web groups projects by. A sandbox credential's projection
	// withholds that path, so the field is ignored for one.
	ProjectPath string `json:"project_path,omitempty"`
	// IDs keeps only the rows with these stable session ids (a deep link to one
	// archived session).
	IDs []string `json:"ids,omitempty"`
}

// SnapshotCursor is a position in a NewestFirst listing.
type SnapshotCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

// SnapshotCapabilities is the subset of Ping's capability bits a SANDBOX
// credential can learn. That credential is not admitted to /v1/health, so a
// client that must know whether this daemon honors a wire field before sending it
// (the --host pick) reads it from the one admitted response instead of probing a
// route the scope denies. Present only for a sandbox credential; its existence
// is itself the proof, since scoped credentials are minted only by daemons that
// already honor every field listed here.
type SnapshotCapabilities struct {
	HostPick        bool `json:"host_pick,omitempty"`
	DeliverHostPick bool `json:"deliver_host_pick,omitempty"`
	TaskHost        bool `json:"task_host,omitempty"`
}

type SnapshotResponse struct {
	Instances []session.InstanceData `json:"instances"`
	// ArchivedTotal is the number of archived rows in the request's repo scope,
	// present only when the request set LazyArchive. Its presence is also how a
	// client learns the daemon understands the lazy-archive request shape.
	ArchivedTotal *int `json:"archived_total,omitempty"`
	// ArchivedByProject counts those archived rows per worktree repo path (the
	// key web groups projects by). Withheld from a sandbox credential.
	ArchivedByProject map[string]int `json:"archived_by_project,omitempty"`
	// NextBefore is set when a Limit cut the listing short: pass it as Before to
	// read the next page.
	NextBefore *SnapshotCursor `json:"next_before,omitempty"`
	// Capabilities: see SnapshotCapabilities. Nil for an operator credential.
	Capabilities *SnapshotCapabilities `json:"capabilities,omitempty"`
	// Caller is the id of the session the presented sandbox credential is bound
	// to (#5571 W3.2). It lets an in-sandbox `whoami` / `archive --self` resolve
	// the caller from the credential instead of from TmuxName, which the narrowed
	// projection withholds. Empty for an operator credential, so every existing
	// client and the local path are unchanged.
	Caller string `json:"caller,omitempty"`
	// BootID is the same per-process daemon identity reported by Ping. A changed
	// value proves requests admitted by the previous process can no longer run.
	BootID string `json:"boot_id,omitempty"`
	// OperationLockTimeoutMS is the admission wait a lifecycle request may spend
	// queued before the daemon can prove whether it started. Browser mutation
	// ledgers consume the live value rather than mirroring a timeout constant.
	OperationLockTimeoutMS int64 `json:"operation_lock_timeout_ms"`
	// OperationClockMS is sampled from the same monotonic clock that bounds the
	// admission wait. Clients use it to pace reconciliation without treating a
	// browser clock as proof of a daemon-side outcome.
	OperationClockMS int64 `json:"operation_clock_ms"`
	// DeliveryAlarms projects persistent watch-task delivery failures into the
	// authoritative snapshot the TUI mirrors (#1238). When a watch task's events
	// have been failing to reach their target session for longer than the alarm
	// threshold — the 2026-07-05 silent event-pipeline outage — the daemon lists
	// them here so the TUI can raise a banner instead of the failure being
	// visible only in the daemon log. Empty (omitted) in the healthy steady
	// state, and additive/gob- and JSON-compatible with older peers that ignore
	// the field.
	DeliveryAlarms []DeliveryAlarm `json:"delivery_alarms,omitempty"`
	// SkippedRepos names repos whose instances.json was corrupted and dropped at
	// daemon startup (#603), so a snapshot served after the drop carries the
	// incompleteness to clients instead of silently substituting a partial
	// session list as the complete answer. The daemon intentionally skips a
	// corrupted per-repo file at startup (logging a WARNING) rather than aborting
	// and orphaning every live session; this field is the wire-side channel that
	// WARNING never had — added when the CLI's list/get/whoami reads were rewired
	// onto the Snapshot RPC (#1029 PR 2), so a CLI user decoding this response
	// can tell "repo has no sessions" apart from "the daemon dropped this repo's
	// sessions at startup" the same way the disk-fallback path already could
	// (#730). Empty (omitted) when no repo was skipped, and additive/gob- and
	// JSON-compatible with older peers that ignore the field.
	//
	// Scoped to the request's RepoID by the handler, mirroring Instances, so a
	// repo-scoped read only reports a skip that is relevant to that scope. Withheld
	// from a sandbox caller: a repo ID is operator identity the sandbox projection
	// already withholds (sandboxSafeInstanceData drops Path/Worktree/TmuxName), and
	// the skip set is named for the operator reading stdout, not the agent reading
	// its own row.
	//
	// Lifecycle: seeded from the startup refresh (refreshDaemonInstances with
	// existing==nil) and trimmed by the polling path to drop repaired repos — a
	// previously-skipped repo whose instances.json now parses AND re-materializes
	// its rows falls out of the set so list/get/whoami stop refusing the
	// now-complete snapshot without waiting for a daemon restart. It never gains
	// a mid-life-corrupted repo: a repo that becomes corrupted mid-life keeps its
	// prior in-memory rows via re-hydration, so its sessions remain in the
	// snapshot and it is correctly NOT reported as skipped until the daemon
	// restarts and re-runs startup. A fresh startup rows-failure (the file
	// parsed but a row failed to load) is WITHHELD from this wire field — master
	// never seeded it, and the refusal names a count with no working remedy —
	// while the startup orphan sweep still defers from the internal skip set;
	// see Manager.startupRowsFailedRepos. See daemon.daemon.go::refreshDaemonInstances
	// and snapshot.go::retainStillSkipped for the skip lifecycle.
	SkippedRepos []SkippedRepo `json:"skipped_repos,omitempty"`
}

// SkippedRepo names one repo the daemon could not load sessions for at startup,
// so the Snapshot RPC can report it to clients rather than silently serving a
// truncated list. See SnapshotResponse.SkippedRepos.
type SkippedRepo struct {
	RepoID string `json:"repo_id"`
	// Reason is a stable machine-readable code for why the repo was skipped:
	// SkippedRepoReasonCorruptedInstancesJSON,
	// SkippedRepoReasonUnreadableInstancesJSON,
	// SkippedRepoReasonNewerSchemaInstancesJSON or
	// SkippedRepoReasonRowsFailedToLoad. A client treats an empty or unknown
	// value as corrupted, which is all an older daemon ever sent.
	Reason string `json:"reason,omitempty"`
	// FailedRows is the number of sessions in the repo whose row the daemon
	// could not load (worktree or tmux gone) when Reason is
	// SkippedRepoReasonRowsFailedToLoad, so the refusal can name how many are
	// hidden and the remedy can target them. Zero for the read/parse reasons,
	// under which a row never reached the materializer (#4876).
	FailedRows int `json:"failed_rows,omitempty"`
}

// SkippedRepoReasonCorruptedInstancesJSON is the reason carried for a repo whose
// instances.json failed to parse at daemon startup (#603).
const SkippedRepoReasonCorruptedInstancesJSON = "corrupted-instances-json"

// SkippedRepoReasonUnreadableInstancesJSON is the reason carried for a repo
// whose instances.json could not be read at all — a permission or I/O failure
// rather than bad JSON (#4783). It is kept apart from the corrupted reason
// because the remedy differs: an unreadable file needs its access restored, not
// its JSON repaired.
const SkippedRepoReasonUnreadableInstancesJSON = "unreadable-instances-json"

// SkippedRepoReasonNewerSchemaInstancesJSON is the reason carried for a repo
// whose instances.json was written by a newer af in a schema this daemon does
// not understand (#4783). Its remedy is to upgrade, and the file must not be
// touched: deleting or "repairing" it discards sessions this binary cannot see.
const SkippedRepoReasonNewerSchemaInstancesJSON = "newer-schema-instances-json"

// SkippedRepoReasonRowsFailedToLoad is the reason carried for a repo whose
// instances.json parsed but some of its rows failed to load — the worktree or
// tmux session a Live row points at is gone, not the file (#4876). It is kept
// apart from the corrupted/unreadable reasons because the file is fine: the
// remedy is to restore or delete the unloadable sessions, not to repair a JSON
// error that is not there. On the POLLING path it rewrites a previously-skipped
// repo's stale reason (corrupted/unreadable) to the accurate one and surfaces
// on the wire. At STARTUP it arms the orphan-sweep deferral (a repo whose rows
// dropped contributes the same zero rows to the protected-slug set as a
// corrupted one, so the sweep must spare its live containers, #4998
// follow-up), but it is WITHHELD from the wire SkippedRepos — master never
// seeded a fresh startup rows-failure, and the refusal names a count with no
// working remedy (restore/kill resolve titles against the in-memory set, which
// excludes the unloadable row); list/get/whoami serve the loadable rows plus a
// log warning instead. See Manager.startupRowsFailedRepos.
const SkippedRepoReasonRowsFailedToLoad = "rows-failed-to-load"

// skippedRepoReasonForReadError names the reason for a repo the loader could
// not read, so the client can give the remedy that fits the failure.
func skippedRepoReasonForReadError(err error) string {
	var newer *config.UnsupportedSchemaVersionError
	if errors.As(err, &newer) {
		return SkippedRepoReasonNewerSchemaInstancesJSON
	}
	return SkippedRepoReasonUnreadableInstancesJSON
}

// SkippedRepos returns the repos dropped at daemon startup due to a
// corrupted/unreadable instances.json, or a file that parsed but whose rows
// failed to load (#603), scoped to repoID (all repos when empty). It is the
// wire surface the Snapshot RPC carries so a CLI list/get/whoami can refuse or
// caveat rather than silently serving a partial list as complete (#730's
// principle extended to the wire surface #1029 PR 2 introduced) — except a
// fresh startup rows-failure, which is withheld (see startupRowsFailedRepos).
//
// Seeded from the most recent startup refresh and trimmed by the polling path
// to drop repaired repos, but never gains a mid-life-corrupted one: a repo
// that becomes corrupted mid-life keeps its prior in-memory rows via
// re-hydration, so its sessions remain in the snapshot and it is correctly
// NOT reported as skipped until the daemon restarts and re-runs startup. A
// previously-skipped repo whose instances.json now parses falls out of the
// set so list/get/whoami stop refusing the now-complete snapshot. See
// daemon.go::refreshDaemonInstances and retainStillSkipped below for the skip
// lifecycle.
func (m *Manager) SkippedRepos(repoID string) []SkippedRepo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.skippedReposScopedLocked(repoID)
}

// seedStartupRowsFailedRepos records the repos whose instances.json PARSED at
// daemon startup but whose rows failed to load (rows-failed-to-load). The
// startup orphan sweep still arms from skippedRepos, but the Snapshot RPC
// withholds these repos from the wire SkippedRepos so list/get/whoami serve
// the loadable rows plus a log warning instead of refusing the whole repo —
// matching master, which never seeded a fresh startup rows-failure (the
// refusal names a count, not a session, and restore/kill resolve titles
// against the in-memory set, which by definition excludes the unloadable row).
// A polling-path previously-skipped repo (corrupted at startup, then
// parses-but-rows-fail on poll) is NOT seeded here, so its rows-failed rewrite
// still surfaces on the wire. The caller MUST hold m.mu.
func (m *Manager) seedStartupRowsFailedRepos(skipped []SkippedRepo) {
	m.startupRowsFailedRepos = make(map[string]bool, len(skipped))
	for _, s := range skipped {
		if s.Reason == SkippedRepoReasonRowsFailedToLoad {
			m.startupRowsFailedRepos[s.RepoID] = true
		}
	}
}

// skippedReposScopedLocked returns a defensive copy of m.skippedRepos scoped to
// repoID (all repos when empty), with a fresh startup rows-failure withheld from
// the wire (startupRowsFailedRepos). The caller MUST hold m.mu: it is the read
// side shared by SkippedRepos (the public wrapper that locks for the standalone
// read) and SnapshotWithSkipped (which calls it inside its own single lock
// acquisition so the instance pointers and the skip set are sampled from the
// same manager generation).
func (m *Manager) skippedReposScopedLocked(repoID string) []SkippedRepo {
	// A repo whose rows failed to load at startup is withheld from the wire:
	// master never seeded a fresh rows-failure, and the refusal names a count
	// with no working remedy (the file is fine; restore/kill resolve titles
	// against the in-memory set, which by definition excludes the unloadable
	// row). The sweep deferral still arms from skippedRepos, so the live
	// container is spared; only the wire refusal is decoupled. See
	// startupRowsFailedRepos.
	//
	// The withholding is gated on the entry's CURRENT reason, not the repo ID
	// alone: a polling refresh can rewrite a startup rows-failure's reason to
	// corrupted/unreadable when the file later fails to read or parse
	// (retainStillSkipped carries the fresh entry's reason), and that is an
	// actionable state with a real remedy the wire must surface — so only a
	// reason still equal to rows-failed-to-load stays suppressed.
	withheld := m.startupRowsFailedRepos
	if repoID == "" {
		if len(m.skippedRepos) == 0 {
			return nil
		}
		out := make([]SkippedRepo, 0, len(m.skippedRepos))
		for _, s := range m.skippedRepos {
			if withheld[s.RepoID] && s.Reason == SkippedRepoReasonRowsFailedToLoad {
				continue
			}
			out = append(out, s)
		}
		return out
	}
	var out []SkippedRepo
	for _, s := range m.skippedRepos {
		if s.RepoID != repoID {
			continue
		}
		if withheld[s.RepoID] && s.Reason == SkippedRepoReasonRowsFailedToLoad {
			continue
		}
		out = append(out, s)
	}
	return out
}

// SnapshotWithSkipped returns the authoritative instance projection together
// with the startup-skipped repo set, both scoped to repoID (all repos when
// empty) and captured under ONE acquisition of m.mu so the two projections
// cannot disagree across a polling refresh.
//
// The instance serialization that follows still happens OUTSIDE m.mu — the
// pointers/pending values are collected under the lock and serialized after
// it — so a slow per-instance ToInstanceData does not block a concurrent
// mutation. Only the snapshot of POINTERS and the copied skip set are taken
// under the lock, exactly what Manager.Snapshot already did for the instance
// half, extended here to read the skip set under the same acquisition.
//
// The single-lock read closes the race separate Snapshot and SkippedRepos
// calls leave open: between Snapshot releasing m.mu and SkippedRepos
// re-acquiring it, a polling refresh can repair a startup-skipped repo —
// loading its now-parseable rows into m.instances and trimming the now-stale
// skip set — so the response would carry the OLD, INCOMPLETE instance list
// with the NEW, CLEARED skip set, and 'sessions list' would again report the
// repaired repo as silently empty instead of refusing the not-yet-complete
// snapshot (#603 over the wire). Reading both under one lock means a refresh
// is fully before or fully after this snapshot, never wedged in between.
// See daemon.go::refreshLocked and retainStillSkipped for the skip lifecycle.
//
// Manager.Snapshot delegates here and discards the skip set; the Snapshot RPC
// handler in this file uses the joint return so SnapshotResponse carries
// projections sampled from the same generation.
func (m *Manager) SnapshotWithSkipped(repoID string) ([]session.InstanceData, []SkippedRepo) {
	m.mu.Lock()
	keys := make([]string, 0, len(m.instances)+len(m.pendingCreates))
	for key := range m.instances {
		if repoID != "" {
			rid, _ := splitDaemonInstanceKey(key)
			if rid != repoID {
				continue
			}
		}
		keys = append(keys, key)
	}
	for key := range m.pendingCreates {
		if _, settled := m.instances[key]; settled {
			continue
		}
		if repoID != "" {
			rid, _ := splitDaemonInstanceKey(key)
			if rid != repoID {
				continue
			}
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type snapshotEntry struct {
		instance *session.Instance
		pending  session.InstanceData
	}
	entries := make([]snapshotEntry, 0, len(keys))
	for _, key := range keys {
		if inst := m.instances[key]; inst != nil {
			entries = append(entries, snapshotEntry{instance: inst})
			continue
		}
		if pending, ok := m.pendingCreates[key]; ok {
			entries = append(entries, snapshotEntry{pending: pending})
		}
	}
	skipped := m.skippedReposScopedLocked(repoID)
	m.mu.Unlock()

	data := make([]session.InstanceData, 0, len(entries))
	for _, entry := range entries {
		projected := entry.pending
		if entry.instance != nil {
			projected = entry.instance.ToInstanceData()
		}
		data = append(data, projected)
	}
	return data, skipped
}

// retainStillSkipped returns the part of the startup skip set prev that this
// refresh has NOT proven repaired. The polling refresh uses it so the set
// shrinks as repos are repaired (#603 closed over the wire) without ever
// GAINING a repo that fails mid-life: those keep their re-hydrated in-memory
// rows and stay out of the skip set until the daemon restarts and re-runs
// startup.
//
// Leaving the set takes positive evidence: the repo is in reread, meaning this
// refresh read AND parsed its instances.json INTO a fully loadable row set —
// refreshDaemonInstances retracts a repo from reread when its file parses but
// ANY row fails fromInstanceDataForRefresh, so a parses-but-some-row-unloadable
// file stays a non-repair too (a partial loss is the same lie as a zero-rows
// one, #4812, #4876). Absence from fresh is not enough (#4783). A repo can be
// missing from fresh because the loader could not read it, or because its
// directory is gone, and neither is a repair; treating the omission as one
// served a truncated snapshot as complete again. A repo fresh still reports
// is kept with fresh's entry, so the reason tracks the file's current state:
// a previously-skipped repo whose file now parses but loses rows is rewritten
// to rows-failed-to-load here (#4876), since the stale corrupted/unreadable
// wording would send the operator to repair a file that is fine.
func retainStillSkipped(prev, fresh []SkippedRepo, reread map[string]bool) []SkippedRepo {
	if len(prev) == 0 {
		return nil
	}
	freshByID := make(map[string]SkippedRepo, len(fresh))
	for _, s := range fresh {
		freshByID[s.RepoID] = s
	}
	var out []SkippedRepo
	for _, s := range prev {
		if f, ok := freshByID[s.RepoID]; ok {
			out = append(out, f)
			continue
		}
		if !reread[s.RepoID] {
			out = append(out, s)
		}
	}
	return out
}

// DeliveryAlarm is one watch task whose event delivery to TargetSession has
// been failing persistently (past watcherDeliveryAlarmThreshold). It carries
// what the TUI banner needs — the target, how many events are stuck, when the
// failures began, and the last error — so an operator sees a dead delivery
// pipeline within the threshold window instead of discovering it by accident
// (#1238 fix c).
type DeliveryAlarm struct {
	TaskID        string `json:"task_id"`
	TaskName      string `json:"task_name"`
	TargetSession string `json:"target_session"`
	// Pending is the number of undelivered events queued behind the failure.
	Pending int `json:"pending"`
	// PendingUnknown marks a queue whose on-disk state could not be loaded
	// (#3242): Pending is 0 because the backlog could not be enumerated, not
	// because it is empty. The banner says "unknown" instead of "0 pending" —
	// a fabricated zero here would tell the operator nothing is stuck while an
	// unreadable file holds the backlog.
	PendingUnknown bool `json:"pending_unknown,omitempty"`
	// Consecutive is the count of back-to-back failed delivery attempts.
	Consecutive int `json:"consecutive"`
	// Since is when the current run of failures began (first consecutive
	// failure); the TUI renders "since HH:MM".
	Since time.Time `json:"since"`
	// LastError is the most recent delivery error, for the banner detail.
	LastError string `json:"last_error,omitempty"`
}

// deliveryAlarms assembles the persistent delivery-failure alarms for a repo
// (empty = all repos) from the watcher supervisor, evaluated against the
// current time. Nil when there is no supervisor or nothing is failing.
func (s *controlServer) deliveryAlarms(repoID string) []DeliveryAlarm {
	if s.watchers == nil {
		return nil
	}
	return s.watchers.deliveryAlarms(repoID, time.Now())
}

// It keeps the two-argument shape net/rpc requires by reflection, and passes a
// bare context — so this path is never narrowed to a sandbox owner.
//
// That is correct rather than a bypass, and worth stating because it does not
// look it: net/rpc is registered on the UNIX SOCKET only (0600, chmod'd at
// creation), which is trusted transport and carries the operator's authority by
// construction. A sandbox never reaches it — its callback dials the HTTP listener,
// where servePosture binds the owner and the route below narrows. If net/rpc is
// ever exposed on a network listener, this wrapper becomes the hole, and the fix
// is to thread a real context rather than to widen the constraint.
func (s *controlServer) Snapshot(req SnapshotRequest, resp *SnapshotResponse) error {
	return s.snapshot(context.Background(), req, resp)
}

// snapshot is Snapshot with the request context, so it can see whether the caller
// is a SANDBOX and narrow the answer to that sandbox's own session (#3056).
//
// This is the first route admitted to the sandbox callback scope, and it carries
// the constraint that admission requires. Snapshot is a deliberate choice rather
// than a convenient one: it is the enumeration that made every other route
// aimable — #3012 denied it for exactly that reason — so if the owner constraint
// is worth anything, it has to hold here. An agent reading its OWN session's state
// is also the most obviously legitimate thing a remote agent does.
//
// THREE narrowings. The first is the obvious one; the other two are each a
// finding, because "show only the caller's own row" is not the same as "show only
// what the caller may see":
//
//   - Instances is filtered to the owner's PROJECT (#5571 W3.5): the repo is the
//     owner's own row's, whatever the request named, and every surviving row is
//     the projection below. Other projects are never visible, and neither is a
//     repo ID. (#3056 narrowed to the owner's single row; the project bound is
//     the W3.5 widening.)
//   - Each surviving row is PROJECTED (sandboxSafeInstanceData). The owner's own
//     row still carries the daemon host's layout — Path is the absolute repo root
//     on the host — so narrowing alone handed back the operator layout that the
//     path-oracle routes were withdrawn from this scope for (#3065 review).
//   - DeliveryAlarms is emptied outright. An alarm names its target by TITLE, so
//     filtering it would mean mapping the owner's id to a title and comparing —
//     an identity conversion in the one place that must not get identity wrong.
//     A sandbox has no use for watch-task delivery diagnostics anyway, so the
//     honest answer is none rather than a mapping that could be off by one repo.
//
// The operator's own callers (unix socket, operator token) are untouched: they
// carry no sandbox owner, so neither narrowing applies.
func (s *controlServer) snapshot(ctx context.Context, req SnapshotRequest, resp *SnapshotResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	// A sandbox credential reads its own PROJECT (#5571 W3.5): the repo filter is
	// the owner's, whatever the request named — the sandbox does not know the
	// daemon host's repo IDs and must not be able to aim at another project's.
	// An owner that is no longer tracked reads nothing rather than everything.
	sandboxOwnerID, isSandbox := sandboxOwner(ctx)
	sandboxProjectID, sandboxProjectErr := "", error(nil)
	if isSandbox {
		var proj sandboxProject
		if proj, sandboxProjectErr = s.manager.sandboxProjectOf(sandboxOwnerID); sandboxProjectErr == nil {
			sandboxProjectID = proj.repoID
			req.RepoID = proj.repoID
		}
	}
	// Sample before the rows so reconciliation delay is measured against the
	// projection returned with this response.
	operationClockMS := operationClockMilliseconds()
	// Read instances AND the skip set in ONE acquisition of m.mu, not as
	// separate Snapshot+SkippedRepos calls, so a polling refresh that repairs a
	// startup-skipped repo between the two cannot leave the response carrying
	// the old, incomplete instance list with the new, cleared skip set — a
	// 'sessions list' would then report the repaired repo as silently empty
	// instead of refusing the not-yet-complete snapshot (#603 over the wire).
	instances, skipped := s.manager.SnapshotWithSkipped(req.RepoID)
	alarms := s.deliveryAlarms(req.RepoID)
	if isSandbox {
		instances = projectRowsForSandbox(instances, sandboxProjectID, sandboxProjectErr == nil)
		resp.Caller = sandboxOwnerID
		// Project only what the sandbox scope HONORS, not the operator's full
		// triple: every bit here is a promise that the daemon honors that wire
		// field for this credential, and the client's preflight refuses a pick
		// when the bit is absent (#5571 W6.3). The scope's AddTask path clears
		// Task.Host (stampSandboxTask), and /v1/DeliverPrompt — the sole bearer
		// of DeliverPromptRequest.Host — is not admitted to the scope at all
		// (no sandboxAllowed), so advertising those two would be a false claim
		// the preflight would trust. HostPick is honored: a sandbox create
		// requires --host and the create path keeps it. Advertising the other
		// two would let `af tasks add --host` pass preflight and be silently
		// dropped, exactly the fail-closed protection the bit exists to produce.
		resp.Capabilities = &SnapshotCapabilities{HostPick: true}
		alarms = nil
		// A repo ID is operator identity a sandbox must not learn — the
		// sandboxSafeInstanceData projection already withholds Path/Worktree/
		// TmuxName for the same reason, and the skip set is named for the
		// operator reading stdout, not the agent reading its own row (#3065).
		skipped = nil
	}
	if isSandbox {
		// The projection above withheld the worktree path these two keys read.
		req.ProjectPath = ""
		// A sandbox reads only its own project and has no business paging an
		// archive: the paging inputs are ignored and no cursor is returned (below),
		// exactly as archived_by_project is withheld.
		req.NewestFirst, req.Before, req.IDs = false, nil, nil
	}
	if err := req.Validate(); err != nil {
		return err
	}
	if req.LazyArchive {
		total, byProject := archivedSummary(instances)
		resp.ArchivedTotal = &total
		if !isSandbox {
			resp.ArchivedByProject = byProject
		}
	}
	instances, next, err := FilterSnapshotPage(req, instances)
	if err != nil {
		return err
	}
	resp.Instances = instances
	if !isSandbox {
		resp.NextBefore = next
	}
	resp.DeliveryAlarms = alarms
	resp.SkippedRepos = skipped
	if s.manager.lifecycle != nil {
		resp.BootID = s.manager.lifecycle.snapshot().bootID
	}
	resp.OperationLockTimeoutMS = opLockTimeout.Milliseconds()
	resp.OperationClockMS = operationClockMS
	return nil
}

// projectRowsForSandbox projects the rows of the caller's project (already
// filtered to it by repo) through the sandbox-safe projection. With no known
// project it returns an EMPTY slice, never the input: the direction a bug here
// should fail in.
func projectRowsForSandbox(instances []session.InstanceData, projectID string, known bool) []session.InstanceData {
	out := make([]session.InstanceData, 0, len(instances))
	if !known || projectID == "" {
		return out
	}
	for _, d := range instances {
		out = append(out, sandboxSafeInstanceData(d))
	}
	return out
}

// onlyOwnedBySandbox keeps just the caller's own session.
//
// Returns an EMPTY slice rather than nil when nothing matches, and never returns
// the input unfiltered: an owner whose session has been archived or killed sees
// nothing, which is correct, and is also the direction a bug here should fail in.
func onlyOwnedBySandbox(instances []session.InstanceData, owner string) []session.InstanceData {
	owned := make([]session.InstanceData, 0, 1)
	for _, d := range instances {
		if d.ID != "" && d.ID == owner {
			owned = append(owned, sandboxSafeInstanceData(d))
		}
	}
	return owned
}

// sandboxSafeInstanceData projects one session row down to what a sandbox may
// learn about ITSELF (#3065 review).
//
// Narrowing to the owner's row is not sufficient, which is the finding: the row
// itself carries the DAEMON HOST's layout. InstanceData.Path is the absolute
// repository root on the host, Worktree carries host paths, TmuxName names a host
// tmux session — none of which exist inside the sandbox, and all of which are
// exactly the operator layout that ListDirectory and the repo-path routes were
// withdrawn from the scope for. A credential correctly restricted to its own row
// still read them.
//
// Built by CONSTRUCTION, not by blanking. Copying d and clearing a denylist would
// leak every field added to InstanceData afterwards, silently, at the moment
// someone else adds one — and this struct has thirty-odd fields and grows. Listing
// what may go out means a new field is withheld until somebody decides otherwise,
// which is the same default-deny the route scope uses. A test walks the projection
// reflectively so a newly populated field has to be an explicit decision.
//
// What is included is the agent's own operational state: who it is, what it is
// running, what state it is in. What is excluded is anything describing the
// daemon HOST'S LAYOUT — the absolute repository path, the worktree, the tmux
// session name — anything about other sessions, and the conversation/prompt
// payloads a sandbox already has locally and does not need served back.
//
// Host is deliberately NOT here. This projection is shared by the Snapshot path
// (projectRowsForSandbox), which projects EVERY session in the owner's project,
// not only the one just created: a sibling session's Host is an operator-chosen
// listed-host alias that this sandbox did not supply, so returning it here would
// leak operator host picks across sessions and would contradict the classCaller
// rationale (caller text is what THIS sandbox sent). The picked host is preserved
// ONLY by the create-specific projection (projectSandboxCreateResponse), where the
// row is the session the sandbox itself just started with --host, and the create's
// post-create guard (api/sessions.go) audits what came BACK against that pick.
// Withholding it from ordinary snapshots does not reintroduce the version-skew
// failure, because the guard reads the create response, not Snapshot.
//
// Program is deliberately NOT here: it is text the sandbox itself supplied (a full
// command, possibly naming a path), the sandbox does not need it served back, and
// returning caller-supplied text is exactly what makes a response depend on the
// host's layout. Every string this returns is classified in sandboxStringClasses.
func sandboxSafeInstanceData(d session.InstanceData) session.InstanceData {
	return session.InstanceData{
		ID:                       d.ID,
		TaskID:                   d.TaskID,
		Title:                    d.Title,
		Branch:                   d.Branch,
		Status:                   d.Status,
		Liveness:                 d.Liveness,
		InFlightOp:               d.InFlightOp,
		LifecycleAction:          d.LifecycleAction,
		CurrentAgent:             d.CurrentAgent,
		BackendType:              d.BackendType,
		TaskRunActive:            d.TaskRunActive,
		LimitResetAt:             d.LimitResetAt,
		IdleReason:               d.IdleReason,
		LastPromptAttemptAt:      d.LastPromptAttemptAt,
		LastPromptDeliveryStatus: d.LastPromptDeliveryStatus,
		LastPaneChurnAt:          d.LastPaneChurnAt,
		CreatedAt:                d.CreatedAt,
		UpdatedAt:                d.UpdatedAt,
	}
}
