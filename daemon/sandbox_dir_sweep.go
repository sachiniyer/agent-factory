package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The startup sweep of sandbox hosts for orphaned session dirs (#5571 W1.7): what
// af created on a host and no persisted row owns is reaped by persisted identity,
// and a workspace that may hold unpushed work is reported, never deleted. The
// proof and the safety rules live in session.SweepSandboxOrphans.

// sandboxOrphanMinAge keeps the sweep off anything a provision might still be
// writing: a dir younger than this is never a candidate.
const sandboxOrphanMinAge = 15 * time.Minute

// sweepSandboxOrphans is a package var so a test can observe the call.
var sweepSandboxOrphans = session.SweepSandboxOrphans

// sweepStartupOrphanSandboxDirs runs the sweep in the background after the
// instances are published. It does not run while any repo is skipped as
// unloadable: the owned set would be incomplete, and a live row mistaken for an
// orphan is exactly the failure this must never cause.
func (m *Manager) sweepStartupOrphanSandboxDirs() {
	m.mu.Lock()
	skipped := len(m.skippedRepos)
	m.mu.Unlock()
	if skipped > 0 {
		log.WarningLog.Printf("sandbox orphan sweep: skipped; %d repo(s) did not load, so the set of owned session dirs is incomplete", skipped)
		return
	}
	go func() {
		findings := sweepSandboxOrphans(session.SweepRowsOf(m.instanceList()), sandboxOrphanMinAge, m.sandboxDirOwned)
		for _, f := range findings {
			switch f.Action {
			case session.SweepReaped:
				log.InfoLog.Printf("sandbox orphan sweep: reaped %s on %s: %s", f.Dir, f.Host, f.Reason)
			default:
				log.WarningLog.Printf("sandbox orphan sweep: %s on %s needs attention: %s", f.Dir, f.Host, f.Reason)
			}
		}
	}()
}

// instanceList snapshots the tracked instances.
func (m *Manager) instanceList() []*session.Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*session.Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, inst)
	}
	return out
}

// sandboxDirOwned re-reads ownership at reap time, so a row that persisted while
// the sweep was running is honored.
func (m *Manager) sandboxDirOwned(dir, ref string) bool {
	for _, row := range session.SweepRowsOf(m.instanceList()) {
		d, r := row.Identity()
		if d == dir || (ref != "" && r == ref) {
			return true
		}
	}
	return false
}
