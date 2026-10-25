package session

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// knownInstanceDataFields is the set of top-level JSON keys a row this binary
// writes or reads. It is the struct's json tags plus the two wire names
// InstanceData.MarshalJSON derives (status_name, liveness_name; see
// state_names.go) — a row carrying a key outside this set was written by a
// newer same-schema binary under a field this version's InstanceData does not
// decode, and decoding the row drops that key. The transient-ghost carve-out
// (isLegacyTransientGhostRow) checks the decoded form, so a Loading/Deleting row
// carrying a newer durable marker on such a field looks marker-less and would be
// reaped; rowHasUnrecognizedInstanceFields lets the save path fail closed and
// preserve the row verbatim instead.
var knownInstanceDataFields = func() map[string]struct{} {
	out := map[string]struct{}{
		"status_name":   {},
		"liveness_name": {},
	}
	collectInstanceDataJSONFields(reflect.TypeOf(InstanceData{}), out)
	return out
}()

func collectInstanceDataJSONFields(t reflect.Type, out map[string]struct{}) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			if f.Type.Kind() == reflect.Pointer {
				f.Type = f.Type.Elem()
			}
			if f.Type.Kind() == reflect.Struct {
				collectInstanceDataJSONFields(f.Type, out)
			}
			continue
		}
		if tag := f.Tag.Get("json"); tag != "" {
			name := strings.SplitN(tag, ",", 2)[0]
			if name != "-" {
				out[name] = struct{}{}
			}
		}
	}
}

// rowHasUnrecognizedInstanceFields reports whether raw carries a top-level JSON
// key this binary's InstanceData does not decode. Such a key may be an additive
// field a newer same-schema binary wrote (a durable marker this version does not
// know); decoding into InstanceData drops it, so the save path must inspect the
// raw form to fail closed and preserve the row verbatim rather than reap it as a
// disposable transient ghost.
func rowHasUnrecognizedInstanceFields(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for k := range fields {
		if _, known := knownInstanceDataFields[k]; !known {
			return true
		}
	}
	return false
}

// isLegacyTransientGhostRow identifies disposable projections, not sessions.
// Older binaries could strand Loading/Deleting rows on disk and every create
// path deliberately treats them as overwritable (#551): backfilling through
// ForStorage would settle their transient status and turn the ghost into a
// real title claim. Durable recovery markers are the exception — those rows
// represent work the daemon still owes — matching the loader-side carve-out
// (daemon.isLegacyTransientGhost in daemon/refresh_support.go) for the
// PendingHandoffMission and RuntimeCleanupStateUnknown markers so the save
// and load paths reap the same rows for those.
//
// The save-side carve-out is STRICTER than the loader-side for the remaining
// durable-retention markers: an unresolved Worktree.RelocationRecovery or a
// non-empty ArchiveReport. snapshotInstancesForCheckpoint treats both as
// durable retention (an in-memory Loading/Deleting row carrying either is kept
// in the checkpoint), and a stranded/version-skewed ghost carrying either is one
// of the unloadable row classes this change is meant to preserve (a malformed
// relocation record is the ghost fixture the e2e test uses). The loader-side
// isLegacyTransientGhost only gates the legacy ID-backfill reap, not row
// deletion, so preserving these rows at save time does not resurrect rows the
// loader reaps — the loader never deletes on-disk rows, it only skips the ID
// backfill for disposable ghosts. A Loading/Deleting row with a relocation
// recovery or archive report therefore stays on disk for repair across the
// checkpoint, matching the in-memory snapshot's durable-retention rule.
//
// An empty ArchiveReport (a non-nil pointer with no RetainedTrees) is treated
// as disposable, matching the in-memory snapshot: snapshotInstancesForCheckpoint
// gates durable retention on archiveReportPending, which ProjectionSnapshot
// sets only when !ArchiveReport.Empty(), and ForStorage writes a nil
// ArchiveReport for an empty report. A Loading/Deleting row carrying an empty
// report therefore has no retained source trees to protect and is the same
// disposable transient ghost as one with no report at all — a Deleting variant
// would otherwise keep reserving its title in findTitleConflictLocked across
// every checkpoint, blocking recreation of that session indefinitely.
func isLegacyTransientGhostRow(item InstanceData) bool {
	return (item.Status == Loading || item.Status == Deleting) &&
		item.PendingHandoffMission == "" && !item.RuntimeCleanupStateUnknown &&
		item.Worktree.RelocationRecovery == nil &&
		(item.ArchiveReport == nil || item.ArchiveReport.Empty())
}

// preserveUnmaterializedGhostRows returns the on-disk rows that no in-memory
// instance represents, in their original raw form, so the wholesale per-repo
// checkpoint rewrite preserves them verbatim instead of silently erasing them.
//
// refreshDaemonInstances deliberately leaves on disk any row
// fromInstanceDataForRefresh cannot materialize: it logs the failure, keeps the
// row counted against its task's cap (#1892), and promises the row "stays on
// disk for repair or manual cleanup" (daemon/daemon.go refresh loop). The
// shutdown checkpoint is the only production full-save; before this merge it
// rewrote the repo file purely from the in-memory instances, so in a repo that
// held at least one normally-loadable session every unloadable ("ghost") row in
// that repo was silently destroyed at daemon shutdown — the only durable
// record for PendingOnComplete teardown obligations (#4162, leaked
// environments), UserKilled tombstones whose kill could then never be finished
// (leaked tmux sessions/worktrees), armed LostRestoreWait retry records
// (#5295), incomplete-archive report rows (the only durable handle to retained
// source trees), and fail-closed relocation-recovery rows — plus loss of the
// ghost's contribution to the watch-task concurrency cap (#1892), letting a
// task exceed max_concurrent_runs after every restart.
//
// "Represented" is by title: titles are unique per repo (the daemon's
// findTitleConflictLocked enforces this on create), so an on-disk row whose
// title matches an in-memory instance is the durable record for that same
// logical session and the in-memory snapshot is authoritative — including a
// killed session the snapshot legitimately drops, which must NOT be
// re-persisted as a ghost (#819/#844). A row whose title is absent from memory
// is a true ghost: preserve it verbatim so the next daemon start can re-read
// it, re-count it against its task's cap, and re-offer repair.
//
// A disposable legacy transient ghost is the one carve-out: an older binary's
// stranded Loading/Deleting projection with no durable retention markers.
// Those rows represent work the daemon never owed (#551), and the loader
// already skips them out of memory, so preserving them here would resurrect
// exactly the ghosts the reap-on-save semantics exist to clear. The carve-out
// matches isLegacyTransientGhostRow and the existing
// TestRepoSaveReapsLegacyLoadingGhost / TestDaemonSaveReapsLegacyLoadingGhost
// contracts.
//
// The rows are returned in their original json.RawMessage form so additive
// fields a newer binary wrote under the same schema version — which this
// binary's InstanceData does not know and would silently drop on an
// Unmarshal/Marshal round trip — survive the checkpoint verbatim (#6). onDisk
// and onDiskRaw are parallel slices: onDisk[i] is the decoded form used for
// matching (title) and the disposable-ghost carve-out, and onDiskRaw[i] is the
// original bytes preserved verbatim. Only the fields needed for matching and
// filtering are decoded; the rest of each retained row is carried through
// untouched.
//
// Duplicate ghost titles collapse to one entry keeping the latest UpdatedAt
// (ties keep the earliest occurrence), matching dedupeInstanceData, so an
// on-disk duplicate a prior write left behind does not survive as two rows.
func preserveUnmaterializedGhostRows(
	onDisk []InstanceData,
	onDiskRaw []json.RawMessage,
	inMemoryTitles map[string]struct{},
) []json.RawMessage {
	if len(onDisk) == 0 {
		return nil
	}
	// Pick one row per title (latest UpdatedAt, ties keep the earliest
	// occurrence, matching dedupeInstanceData).
	keep := make(map[string]int)
	for i, d := range onDisk {
		if _, represented := inMemoryTitles[d.Title]; represented {
			continue
		}
		if isLegacyTransientGhostRow(d) {
			// A Loading/Deleting row with no KNOWN durable markers may still carry
			// a durable marker a newer same-schema binary wrote under a field this
			// version's InstanceData does not decode. Decoding the row drops that
			// field so isLegacyTransientGhostRow sees no marker, but the raw form the
			// checkpoint preserves still has it. Fail closed for a version-skewed
			// transient row: preserve it verbatim so the next start's newer binary
			// can read it, rather than reaping the only recovery record.
			if i < len(onDiskRaw) && rowHasUnrecognizedInstanceFields(onDiskRaw[i]) {
				// fall through to keep the row.
			} else {
				continue
			}
		}
		if prev, ok := keep[d.Title]; ok && !d.UpdatedAt.After(onDisk[prev].UpdatedAt) {
			continue
		}
		keep[d.Title] = i
	}
	if len(keep) == 0 {
		return nil
	}
	ghosts := make([]json.RawMessage, 0, len(keep))
	for i, d := range onDisk {
		idx, ok := keep[d.Title]
		if !ok || idx != i {
			continue
		}
		var raw json.RawMessage
		if i < len(onDiskRaw) {
			raw = onDiskRaw[i]
		} else {
			// A caller that decoded without keeping the parallel raw bytes
			// (tests) falls back to the decoded form, which is byte-identical
			// for rows this binary wrote.
			raw, _ = json.Marshal(d)
		}
		ghosts = append(ghosts, raw)
	}
	return ghosts
}

// checkpointReadError classifies the error from a checkpoint's on-disk read of
// a repo's instances.json. It returns the error to propagate, or nil when the
// checkpoint may proceed by overwriting the file from authoritative memory.
//
// A newer-schema instances.json is a hard refusal, not a repair candidate:
// this binary cannot read it, so overwriting would discard fields and ghost
// rows a newer af wrote (#4783). The remedy is to upgrade, not to clobber, and
// the previous archive-reconciliation path propagated every read error —
// preserve that refusal for the version-skewed handoff rather than routing it
// through the corrupt-but-readable overwrite below.
//
// A genuine I/O/read failure must not clobber rows the checkpoint could not
// read, even if an immediate retry reads the file (#766/#4). The original read
// the checkpoint used failed, so overwriting from the stale in-memory snapshot
// would erase disk-only ghost rows under precisely the intermittent-read
// scenario the read-failure contract exists to protect; the safe action is to
// abort and leave the file for the next start rather than to overwrite from
// memory. An I/O error here is the os.ReadFile *fs.PathError LoadRepoInstances
// wraps when its own read fails — distinct from the content/schema errors
// below, which come from parsing a file that was readable.
//
// A readable file with a present-but-uninterpretable schema_version (null, a
// string, a negative, or an overflowing value that DetectJSONSchemaVersion
// rejects without returning UnsupportedSchemaVersionError) is a possibly-newer
// file this binary cannot read. Refuse to clobber it, mirroring
// refuseStateDowngrade's present-but-uninterpretable rule: the value might come
// from a newer af, so the remedy is to upgrade rather than to repair from
// memory (#5). A corrupt-but-readable file (truncated JSON, an unparseable
// envelope, or a structurally invalid array) falls back to overwrite (#938
// sole-writer contract): the daemon is the sole writer and repairs it from the
// authoritative in-memory snapshot.
func checkpointReadError(readErr error, path string) error {
	var newer *config.UnsupportedSchemaVersionError
	if errors.As(readErr, &newer) {
		return readErr
	}
	var pathErr *fs.PathError
	if errors.As(readErr, &pathErr) {
		return readErr
	}
	raw, ioErr := os.ReadFile(path)
	if ioErr != nil {
		return readErr
	}
	if present, _, verr := config.OnDiskSchemaVersion(raw); present && verr != nil {
		return readErr
	}
	return nil
}

// decodeOnDiskRows decodes a checkpoint's raw on-disk instances array into both
// the decoded form (onDisk) used for matching, the disposable-ghost carve-out,
// and archive reconciliation, and the parallel raw form (onDiskRaw) preserved
// verbatim so additive fields a newer binary wrote under the same schema version
// survive the checkpoint (#6). A corrupt array yields no rows and no verbatim
// ghosts: the daemon is the sole writer and overwrites rather than aborts
// (#938), so there is nothing to reconcile and nothing to preserve.
func decodeOnDiskRows(raw json.RawMessage) (onDisk []InstanceData, onDiskRaw []json.RawMessage) {
	if len(raw) == 0 {
		return nil, nil
	}
	if json.Unmarshal(raw, &onDiskRaw) != nil {
		onDiskRaw = nil
	}
	if json.Unmarshal(raw, &onDisk) != nil {
		onDisk = nil
	}
	return onDisk, onDiskRaw
}

// marshalCheckpointInstances serializes a repo's checkpoint: the in-memory +
// archive-reconciled rows (deduped by title) followed by the verbatim raw ghost
// rows preserveUnmaterializedGhostRows selected. Ghost titles are disjoint from
// the in-memory set, so dedupeInstanceData has already settled the in-memory
// rows and the ghosts append without collision. The ghosts are appended as
// their original json.RawMessage so additive fields a newer binary wrote under
// the same schema version survive the checkpoint (#6).
func marshalCheckpointInstances(group []InstanceData, ghosts []json.RawMessage) (json.RawMessage, error) {
	deduped := dedupeInstanceData(group)
	// Sized from deduped alone, not len(deduped)+len(ghosts). Both operands are
	// lengths of in-memory slices, so that sum cannot realistically overflow —
	// but CodeQL's allocation-size-overflow rule reports the unchecked addition
	// as a high-severity finding and one new high alert fails the required
	// CodeQL check. Dropping the sum removes the flagged expression; append grows
	// the slice for the verbatim ghost rows.
	out := make([]json.RawMessage, 0, len(deduped))
	for i := range deduped {
		b, err := json.Marshal(deduped[i])
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	out = append(out, ghosts...)
	return json.Marshal(out)
}
