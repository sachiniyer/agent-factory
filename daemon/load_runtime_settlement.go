package daemon

import (
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// persistLoadRuntimeReplacements makes every load-time session fact durable
// before the restored map becomes authoritative and stages task-row outcomes for
// publication immediately after installation. Besides replacements observed in
// this process, it reconstructs the durable interruption outbox left by a daemon
// that exited between the session and task writes. The session layer has already
// distinguished an agent respawn from a sibling-tab respawn and applied the same
// unprompted-runtime task-run rule used by restore-time replacement. A failed
// instance write joins that task outcome in the same retry entry. An unprompted
// agent replacement reaches this function with its close already checkpointed:
// the loader writes it after tmux proves the old pane absent and before tmux may
// spawn. The fallback fence remains for injected/standalone constructors that do
// not supply that daemon checkpoint.
func persistLoadRuntimeReplacements(instances map[string]*session.Instance) []settleOwedEntry {
	var owed []settleOwedEntry
	for key, instance := range instances {
		replacement := instance.ConsumeLoadRuntimeReplacement()
		pendingRun, interruptionPending := instance.PendingTaskRunInterruption()
		if !replacement.Replaced && !interruptionPending {
			continue
		}
		repoID, _ := splitDaemonInstanceKey(key)
		entry := settleOwedEntry{repoID: repoID, key: key, instance: instance}
		if interruptionPending {
			entry.interruptedTaskRun = &pendingRun
		}
		if replacement.StartupFailure != nil {
			log.WarningLog.Printf(
				"session %q could not start after its interrupted task run was made durable; retained it for Lost recovery and queued last_run_status publication: %v",
				instance.Title, replacement.StartupFailure)
		} else if replacement.TaskRunInterrupted && !replacement.TaskRunInterruptionCheckpointed {
			log.WarningLog.Printf(
				"load-time agent replacement for %q did not receive task %s's run prompt; closed the run, retained the session, skipped on_complete, and queued last_run_status for durable publication",
				instance.Title, pendingRun.TaskID)
		} else if interruptionPending {
			log.WarningLog.Printf(
				"session %q has a task %s interruption outcome left by an earlier daemon; queued last_run_status for durable publication",
				instance.Title, pendingRun.TaskID)
		}
		fenced := false
		if replacement.Replaced && replacement.TaskRunInterrupted {
			if err := instance.FenceLoadRuntimeReplacementUntilSettlement(); err != nil {
				cleanupErr := instance.TeardownFencedLoadRuntimeReplacement()
				log.WarningLog.Printf(
					"load-time agent replacement for %q could not establish its interrupted-run durability fence (%v); refused the replacement (cleanup: %v) and queued the session checkpoint for retry",
					instance.Title, err, cleanupErr)
				entry.persistInstance = true
			} else {
				fenced = true
			}
		}
		if replacement.Replaced {
			if err := persistInstanceData(repoID, instance.ToInstanceData()); err != nil {
				if fenced {
					cleanupErr := instance.TeardownFencedLoadRuntimeReplacement()
					log.WarningLog.Printf(
						"load-time agent replacement for %q could not persist the interrupted run; refused the replacement and queued the checkpoint for retry (write: %v; cleanup: %v)",
						instance.Title, err, cleanupErr)
				} else {
					log.WarningLog.Printf("load-time runtime replacement for %q could not persist its timestamp and idle evidence; the daemon will retry: %v", instance.Title, err)
				}
				entry.persistInstance = true
			} else if fenced {
				released, err := instance.ReleaseRuntimeReplacementAfterSettlement()
				if err != nil {
					log.WarningLog.Printf("load-time agent replacement for %q could not lower its durability fence after checkpointing the interrupted run; the daemon will retry: %v", instance.Title, err)
					entry.persistInstance = true
				} else if released {
					// The first write made the run close restart-durable. Persist the
					// now-visible replacement separately; failure here is conservative
					// because disk still says Lost with the run already closed.
					if err := persistInstanceData(repoID, instance.ToInstanceData()); err != nil {
						log.WarningLog.Printf("load-time agent replacement for %q is safe and live but its visible state could not be checkpointed; the daemon will retry: %v", instance.Title, err)
						entry.persistInstance = true
					}
				}
			}
		}
		if entry.persistInstance || entry.interruptedTaskRun != nil {
			owed = append(owed, entry)
		}
	}
	return owed
}

// registerLoadRuntimeSettlementsLocked enrolls failed startup writes after the
// corresponding instances have been installed. Caller holds m.mu.
func (m *Manager) registerLoadRuntimeSettlementsLocked(owed []settleOwedEntry) {
	if len(owed) > 0 && m.settleOwed == nil {
		m.settleOwed = make(map[string]settleOwedEntry)
	}
	for _, entry := range owed {
		merged := m.owedSettlementEntryLocked(entry.repoID, entry.key, entry.instance)
		merged.persistInstance = merged.persistInstance || entry.persistInstance
		if entry.interruptedTaskRun != nil {
			merged.interruptedTaskRun = entry.interruptedTaskRun
		}
		m.storeOwedSettlementLocked(merged)
	}
}
