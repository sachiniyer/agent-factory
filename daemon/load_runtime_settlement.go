package daemon

import (
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// persistLoadRuntimeReplacements makes a load-time respawn's timestamp and any
// agent evidence clear durable before the restored map becomes authoritative.
// A failed write is returned as an owed settlement so the live daemon retries it every poll;
// abandoning the newly spawned process would be worse than retaining it with a
// loudly tracked metadata gap.
func persistLoadRuntimeReplacements(instances map[string]*session.Instance) []settleOwedEntry {
	var owed []settleOwedEntry
	for key, instance := range instances {
		if !instance.ConsumeLoadRuntimeReplacement() {
			continue
		}
		repoID, _ := splitDaemonInstanceKey(key)
		if err := persistInstanceData(repoID, instance.ToInstanceData()); err != nil {
			log.WarningLog.Printf("load-time runtime replacement for %q could not persist its timestamp and idle evidence; the daemon will retry: %v", instance.Title, err)
			owed = append(owed, settleOwedEntry{repoID: repoID, key: key, instance: instance})
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
		m.settleOwed[stableSessionKey(entry.repoID, entry.instance)] = entry
	}
}
