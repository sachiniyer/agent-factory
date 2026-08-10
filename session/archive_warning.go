package session

// ReconcileArchiveWarning mirrors the daemon's bounded incomplete-archive
// notice onto an existing client projection. It is independent of liveness: a
// Lost row can acquire the warning during repair, and a repaired row can clear
// it without any status transition.
func (i *Instance) ReconcileArchiveWarning(warning string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.archiveWarning == warning {
		return false
	}
	i.archiveWarning = warning
	return true
}
