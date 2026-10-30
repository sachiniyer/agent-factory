package daemon

import (
	"sort"

	"github.com/sachiniyer/agent-factory/session"
)

func daemonInstanceKey(repoID, title string) string {
	return repoID + "\x00" + title
}

// splitDaemonInstanceKey is the inverse of daemonInstanceKey: it splits a
// "<repoID>\x00<title>" key back into (repoID, title). A key with no NUL
// separator (unexpected) is returned as ("", key).
func splitDaemonInstanceKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}

func daemonInstances(instanceMap map[string]*session.Instance) []*session.Instance {
	keys := make([]string, 0, len(instanceMap))
	for key := range instanceMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	instances := make([]*session.Instance, 0, len(keys))
	for _, key := range keys {
		instances = append(instances, instanceMap[key])
	}
	return instances
}
