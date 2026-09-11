package daemon

import (
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// persistLoadRuntimeReplacements makes every load-time session fact durable
// before the restored map becomes authoritative and stages task-row outcomes for
// publication immediately after installation. The session layer has already
// distinguished an agent respawn from a sibling-tab respawn and applied the same
// unprompted-runtime task-run rule used by restore-time replacement. A failed
// instance write joins that task outcome in the same retry entry; abandoning the
// spawned process would be worse than retaining it with a loudly tracked gap.
func persistLoadRuntimeReplacements(instances map[string]*session.Instance) []settleOwedEntry {
	var owed []settleOwedEntry
	for key, instance := range instances {
		replacement := instance.ConsumeLoadRuntimeReplacement()
		if !replacement.Replaced {
			continue
		}
		repoID, _ := splitDaemonInstanceKey(key)
		entry := settleOwedEntry{repoID: repoID, key: key, instance: instance}
		if replacement.TaskRunInterrupted {
			run := replacement.InterruptedTaskRun
			entry.interruptedTaskRun = &run
			log.WarningLog.Printf(
				"load-time agent replacement for %q did not receive task %s's run prompt; closed the run, retained the session, skipped on_complete, and queued last_run_status for durable publication",
				instance.Title, run.TaskID)
		}
		if err := persistInstanceData(repoID, instance.ToInstanceData()); err != nil {
			log.WarningLog.Printf("load-time runtime replacement for %q could not persist its timestamp and idle evidence; the daemon will retry: %v", instance.Title, err)
			entry.persistInstance = true
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
