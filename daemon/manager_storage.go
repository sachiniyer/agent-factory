package daemon

import "github.com/sachiniyer/agent-factory/session"

func (m *Manager) RefreshInstances() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshLocked()
}

func (m *Manager) InstancesSnapshot() []*session.Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return daemonInstances(m.instances)
}

func (m *Manager) SaveInstances() error {
	return m.storage.SaveInstances(m.InstancesSnapshot())
}

func (m *Manager) SaveInstancesForShutdown() error {
	return m.storage.SaveInstancesForShutdown(m.InstancesSnapshot())
}
