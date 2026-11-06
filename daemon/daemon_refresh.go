package daemon

import (
	"encoding/json"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// refreshDaemonInstances materializes the daemon's in-memory instance map from
// disk. It ALSO returns the ghost task runs: persisted rows whose task run is
// still in flight but which could not be turned into an Instance (#1892).
//
// The second return exists because m.instances is NOT the universe. A row that
// fails to materialize is skipped here and is invisible to anything that walks the
// map — but the agent it describes may well still be running, and the run it
// belongs to is still in flight by the only definition that matters (the
// persisted marker says so). The watch-task concurrency cap counts by walking
// m.instances, so without this a task whose sessions failed to load would admit
// replacements past max_concurrent_runs on every daemon restart, which is exactly
// the restart-survival guarantee the cap is built on.
//
// Keyed by taskRunReservationKey(repoID, taskID) → count, so the cap can add them
// to its projection without re-reading disk. Recomputed on every refresh, so a
// row that starts loading again stops being a ghost — the same self-healing
// projection discipline as the rest of the count.
//
// The fifth return, reread, names every repo whose instances.json this call
// read AND parsed INTO a fully loadable row set. It is the only evidence that
// clears a repo from the skip set (retainStillSkipped): a repo the loader
// could not read, that is absent from disk, or that parses-but-yields-any-
// unloadable-row (retracted below) is not in it, so neither an omission, a
// zero-rows file, nor a partial loss is a repair (#4783, #4812, #4876).
func refreshDaemonInstances(existing map[string]*session.Instance) (map[string]*session.Instance, map[string]int, []SkippedRepo, map[string]bool, error) {
	load, err := loadAllRepoInstancesForRefresh()
	if err != nil {
		return existing, nil, nil, nil, err
	}
	allInstances, unreadable, missing, sigs := load.Instances, load.Skipped, load.Missing, load.Signatures

	next := make(map[string]*session.Instance)
	ghostTaskRuns := make(map[string]int)
	// skipped collects repos whose instances.json failed to read, parse, or
	// whose rows failed to load, so the startup orphan sweep can defer its
	// destructive pass until the protected-slug set is complete and the
	// Snapshot RPC can carry the corrupted/unreadable drop to clients instead
	// of silently serving a partial list (#603 closed over the wire). Collected
	// on every refresh, but only the startup call (existing==nil) genuinely
	// drops rows — the polling path re-hydrates a corrupted repo's prior
	// in-memory rows, so its sessions stay in the snapshot and the caller
	// (restoreInstances vs refreshLocked) decides whether the set is
	// authoritative for the snapshot: seed at startup, then trim repaired repos
	// on poll without ever adding a mid-life-corrupted one (see
	// retainStillSkipped). A fresh startup rows-failure is withheld from the
	// wire by startupRowsFailedRepos (master never seeded it); the polling
	// rewrite of a previously-skipped repo's stale reason still surfaces.
	var skipped []SkippedRepo
	// An unreadable repo is skipped exactly like a corrupt one, with its own
	// reason so the refusal can say "could not be read" rather than "corrupted"
	// (#4783). Its rows, like a corrupt repo's, are re-hydrated from existing on
	// the poll by the absent-repo pass below, since the loader left it out of
	// allInstances.
	unreadableRepos := make(map[string]bool, len(unreadable))
	for _, skip := range unreadable {
		log.WarningLog.Printf("daemon skipping repo %s: unreadable instances.json: %s", skip.RepoID, skip)
		skipped = append(skipped, SkippedRepo{RepoID: skip.RepoID, Reason: skippedRepoReasonForReadError(skip.Err)})
		unreadableRepos[skip.RepoID] = true
	}
	reread := make(map[string]bool, len(allInstances))
	for repoID, raw := range allInstances {
		sig := sigs[repoID]
		var data []session.InstanceData
		var keys []string
		if outcome, ok := refreshRowOutcomes.get(repoID, sig); ok {
			// The file's signature is unchanged, so its parse verdict and row
			// set are the ones recorded when it was read — replay them and let
			// the row loop below re-run its per-tick materialization against
			// `existing` exactly as it would on freshly decoded rows (#5169).
			switch {
			case outcome.corruptErr != nil:
				skipped = recordCorruptedRepo(repoID, outcome.corruptErr, existing, next, skipped)
				continue
			case outcome.empty:
				reread[repoID] = !missing[repoID]
				continue
			default:
				data, keys = outcome.rows, outcome.keys
				reread[repoID] = true
			}
		} else {
			if raw == nil || string(raw) == "[]" || string(raw) == "null" {
				// A missing file loads as "[]" too, but nothing was read, so it
				// clears nothing (#4783).
				reread[repoID] = !missing[repoID]
				refreshRowOutcomes.store(repoID, repoRowsOutcome{sig: sig, empty: true})
				continue
			}

			if err := json.Unmarshal(raw, &data); err != nil {
				// Skip corrupted per-repo JSON instead of failing the whole
				// refresh (#603). At startup (existing==nil) a single corrupt
				// file used to abort NewManager and orphan every live session
				// across every repo. On the polling path we also
				// re-hydrate this repo's prior in-memory instances so a
				// transient/persistent corruption doesn't silently drop
				// already-running sessions — matching the pre-fix semantics
				// of returning `existing` on parse failure.
				//
				// A corrupt file yields NO rows, so its task runs cannot be counted as
				// ghosts either — there is nothing to read a task_id out of. On the poll
				// path the re-hydrated instances above keep counting; at startup
				// (existing==nil) this repo's runs are genuinely unknowable until the file
				// is repaired, and a capped task there may over-admit. That is the one hole
				// the ghost count cannot close, and it is bounded by the same corruption
				// that already costs the repo its whole session list.
				refreshRowOutcomes.store(repoID, repoRowsOutcome{sig: sig, corruptErr: err})
				skipped = recordCorruptedRepo(repoID, err, existing, next, skipped)
				continue
			}
			reread[repoID] = true
			keys = make([]string, len(data))
			for i := range data {
				keys[i] = daemonInstanceKey(repoID, data[i].Title)
			}
			refreshRowOutcomes.store(repoID, repoRowsOutcome{sig: sig, rows: data, keys: keys})
		}
		// Retractable below: a parses-but-any-row-unloadable file is not a
		// repair, or list/get/whoami silently serve a partial list as the
		// complete answer the skip set exists to prevent — a partial loss is
		// the same lie as a total one (#4812, #4876, the "read AND parsed" trim
		// left open here).
		materialized, failedRows := 0, 0
		for i, item := range data {
			key := keys[i]
			if item.ID == "" && !isLegacyTransientGhost(item) {
				item.ID = session.NewInstanceID()
				if existing != nil {
					if prior := existing[key]; prior != nil && prior.ID != "" {
						item.ID = prior.ID
					}
				}
				if err := persistLegacyInstanceID(repoID, item); err != nil {
					log.WarningLog.Printf("daemon skipping legacy instance %q: could not durably assign a stable identity: %v", item.Title, err)
					if existing != nil {
						if prior := existing[key]; prior != nil {
							next[key] = prior
							materialized++
							continue
						}
					}
					// A legacy row whose stable ID could not be durably persisted
					// and has no prior to re-hydrate is dropped — count it as a
					// failed row so the startup skip set is seeded and the
					// destructive orphan sweep defers, exactly like a row that
					// fails fromInstanceDataForRefresh (#4998). Without this, a
					// read-only state file or a full disk at startup leaves the
					// row's slug absent from the protected set and the sweep can
					// reap its still-running container.
					failedRows++
					if rawTaskRunHoldsSlot(item) {
						ghostTaskRuns[taskRunReservationKey(repoID, item.TaskID)]++
					}
					continue
				}
			}
			if existing != nil {
				if instance := existing[key]; instance != nil {
					next[key] = instance
					if stableIDMatchesForDaemon(instance.ID, item.ID) {
						materialized++
					}
					continue
				}
			}

			instance, err := fromInstanceDataForRefresh(item)
			if err != nil {
				failedRows++
				log.WarningLog.Printf("daemon skipping instance %q: %v", item.Title, err)
				// A marked row that cannot materialize still owes its teardown
				// (#4162) — the obligation is durable but nothing in memory can
				// drain it. Name the session so the leak is a visible diagnosis,
				// not the silent disappearance the marker exists to fix.
				if item.PendingOnComplete != nil {
					log.WarningLog.Printf("daemon: session %q is owed an on_complete teardown for task %s (filed %s) but its record failed to load; it stays on disk for repair or manual cleanup: %v",
						item.Title, item.PendingOnComplete.TaskID, item.PendingOnComplete.FiledAt.Format(time.RFC3339), err)
				}
				// The row is invisible to everything that walks m.instances from here on
				// — but its agent may still be running, and its task run is still in
				// flight if the persisted marker says so. Keep it counted against the
				// task's cap (#1892), or a session we merely failed to LOAD would let the
				// task admit a replacement beyond its limit.
				// StartupStateUnknown is terminal by definition. New writers clear
				// TaskRunActive on that transition, but keep this raw-row projection
				// defensive for a record written by an intermediate/older build: a
				// contradictory stale bit must not wedge max_concurrent_runs forever.
				// UserKilled is terminal in the same way and matters more here, because
				// the ghost path is the one place its slot can never be released
				// otherwise: holdsTaskRunSlot already frees a tombstoned INSTANCE (via
				// canAutoRestoreLostSession — "finish-this-kill, never restore-this"),
				// but the bit is only cleared by finishing the kill, and finishUserKill
				// runs against m.instances, which is exactly where a ghost is absent. A
				// tombstoned row that stops loading would therefore hold its slot for
				// good and park every later event for that task (#2418).
				// LostRestoreFailure is likewise terminal: the retry loop deliberately
				// released the slot after giving up, so an unloadable copy cannot reclaim
				// it as a ghost on the next daemon start (#3310).
				if rawTaskRunHoldsSlot(item) {
					ghostTaskRuns[taskRunReservationKey(repoID, item.TaskID)]++
					log.WarningLog.Printf("watch task %s: session %q failed to load but its run is still counted against max_concurrent_runs (#1892); kill or repair the session to release its slot", item.TaskID, item.Title)
				}
				continue
			}
			next[key] = instance
			materialized++
		}
		// Parsed but not fully loadable is NOT a repair: retract the "repaired"
		// signal so the repo stays skipped, and record a rows-failed skip entry
		// whose reason rewrites the stale one on poll and arms the startup
		// orphan-sweep deferral (#4812, #4876, #4998 follow-up). A fresh startup
		// rows-failure is withheld from the wire by startupRowsFailedRepos
		// (master never seeded it); only the polling rewrite of a previously-
		// skipped repo surfaces on the wire. The partial-loss and self-healing
		// rationale lives in the helper.
		skipped = retractRereadOnUnloadableRows(reread, repoID, len(data), materialized, failedRows, skipped)
	}
	refreshRowOutcomes.prune(allInstances)

	// Preserve in-memory instances whose repo storage vanished from disk (#736):
	// either the repo's instances directory was deleted entirely OR the repo's
	// instances.json file was deleted while its directory remains. A deleted
	// directory makes the repo simply absent from allInstances, while a deleted
	// FILE is still reported by the loader as present-but-empty ("[]") with
	// Missing[repoID]=true — so the empty branch above calls continue without
	// touching `existing` either way, and `next` would otherwise drop the repo's
	// live in-memory sessions. The mid-life write path opens up a second arm: when
	// a preserved session next performs any targeted write, UpdateRepoInstances
	// reads the still-missing file as "[]" and unconditionally saves that result
	// back, so the file reappears on disk as a real empty "[]" even though
	// persistence returned "not found". On the following poll Missing[repoID] is
	// false and the repo is present in allInstances, so without provenance the
	// guard would skip preservation and drop every formerly preserved live session
	// again — preserving only until the next write. To keep the protection alive
	// after the file is recreated WITHOUT also preserving a repo that was
	// LEGITIMATELY emptied (another process killed the last session, or an
	// administrator replaced the roster with "[]"), a present-but-empty file is
	// preserved only when the daemon previously OBSERVED it as missing
	// (preservedMissingFileRepos). A genuinely empty file the daemon never saw
	// vanish is the authoritative state, so its stale in-memory sessions are
	// dropped rather than held forever. A present repo that carries REAL rows is
	// left to the normal load path (an external rewrite that replaces the rows is
	// observed, not papered over) and clears any stale provenance so a later
	// legitimate emptying is not over-preserved. Both the missing-directory and
	// missing-file cases are recoverable disk inconsistencies — SaveInstances
	// recreates missing repo directories and files — so we re-hydrate the prior
	// instances and log loudly rather than silently abandoning running sessions.
	// This parallels the corrupted-JSON handling above, which also re-hydrates
	// from `existing`, and keeps the repo's in-flight task runs counted against
	// max_concurrent_runs via the preserved instance itself (#1892). On startup
	// (existing == nil) there is nothing to preserve.
	if existing != nil {
		warnedRepos := make(map[string]bool)
		preservedMissingFileRepos.mu.Lock()
		// Record which repos we OBSERVED as missing this poll, so a file that is
		// later recreated as an empty "[]" by a preserved session's targeted
		// write stays protected on the next tick via provenance rather than by
		// treating every present empty file as data loss. A deleted FILE is
		// reported by the loader as present-but-empty with Missing[repoID]=true,
		// while a deleted DIRECTORY is simply absent from allInstances (the
		// loader only enumerates existing per-repo directories, so the repo is
		// never enumerated and Missing[repoID] is never set). Both vanish-modes
		// are recoverable disk inconsistencies with identical targeted-write
		// recreation mechanics (an atomic write always MkdirAll's the parent),
		// so both feed the same provenance + restore path: a repo the loader
		// neither enumerated (!ok) nor flagged missing is exactly the
		// directory-deleted set, and its preserved-by-absence sessions are
		// precisely what the preserve loop's ok==false arm carries. A repo that
		// the loader could not READ (transient failure) is also absent from
		// allInstances, but it is NOT a deletion: its file still exists, so it is
		// tracked in unreadableRepos and must NOT accrue absence provenance —
		// otherwise restoreMissingRecordsLocked would checkpoint the in-memory
		// instances over a file that was never missing (overwriting newer
		// fields), or retry a misleading missing-record restore every poll on a
		// persistent failure. Unreadable repos are still preserved in memory by
		// the preserve loop's ok==false arm below without provenance, exactly as
		// they were before this change.
		for key := range existing {
			repoID, _ := splitDaemonInstanceKey(key)
			_, observedRepo := allInstances[repoID]
			if missing[repoID] || (!observedRepo && !unreadableRepos[repoID]) {
				preservedMissingFileRepos.oweLocked(repoID, key)
			}
		}
		// Drop provenance for repos whose records are all back on disk, so a later
		// legitimate emptying is not over-preserved. Real rows alone are not
		// enough: the restore checkpoint omits in-flight rows (a Deleting or
		// pre-push archive), which stay protected until their record lands.
		for repoID := range preservedMissingFileRepos.repos {
			if raw, ok := allInstances[repoID]; ok && !missing[repoID] && !(raw == nil || string(raw) == "[]" || string(raw) == "null") && preservedMissingFileRepos.landedLocked(repoID, existing, next) {
				preservedMissingFileRepos.forgetLocked(repoID)
			}
		}
		// Snapshot the owed identities under the lock so the preserve loop reads
		// a stable view.
		owedKeys := preservedMissingFileRepos.owedSnapshotLocked()
		preservedMissingFileRepos.mu.Unlock()
		for key, inst := range existing {
			repoID, _ := splitDaemonInstanceKey(key)
			raw, ok := allInstances[repoID]
			// Skip preservation only when the repo is present, NOT missing, AND
			// either carries real rows on disk or was never observed missing
			// while empty. Absent (deleted directory) and flagged missing
			// (deleted file) are recoverable disk inconsistencies we re-hydrate
			// from `existing`. A present-but-empty file is preserved only with
			// provenance (it was observed missing then recreated empty by a
			// preserved session's targeted write); a present-but-empty file the
			// daemon never saw vanish is the authoritative state and its stale
			// in-memory sessions are dropped.
			if ok && !missing[repoID] {
				if raw == nil || string(raw) == "[]" || string(raw) == "null" {
					if !owedKeys[key] {
						continue
					}
					// Present-but-empty AND previously observed missing: keep
					// protecting the recreated-empty file.
				} else if !owedKeys[key] || next[key] != nil {
					// Real rows: normal load path. Under provenance, an
					// existing session whose record never landed (omitted by
					// the restore checkpoint) is still preserved.
					continue
				}
			}
			if !warnedRepos[repoID] && !unreadableRepos[repoID] {
				log.WarningLog.Printf("daemon preserving in-memory instances for missing repo directory or missing/empty instances.json: %s", repoID)
				warnedRepos[repoID] = true
			}
			next[key] = inst
		}
	}

	return next, ghostTaskRuns, skipped, reread, nil
}
