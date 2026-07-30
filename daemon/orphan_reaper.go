package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// sweepOrphanContainers reaps docker session containers this daemon leaked, at
// startup (#2194 slice 4). Package-level so tests can stub out the docker work.
var sweepOrphanContainers = session.SweepOrphanContainers

// configDirForReap resolves the AF home the orphan sweep scopes its container
// query to. It must match the af.home label runContainer stamps.
var configDirForReap = config.GetConfigDir

// sweepStartupOrphanContainers runs after instance restore, but before the
// manager readiness barrier opens. Therefore every af.home-scoped container the
// sweep can see predates create admission; a new CreateSession cannot manufacture
// a candidate midway through the destructive pass (#2632).
func sweepStartupOrphanContainers(manager *Manager) {
	homeID, err := configDirForReap()
	if err != nil {
		log.WarningLog.Printf("orphan sweep: cannot resolve the AF home; skipping: %v", err)
		return
	}
	sweepOrphanContainers(homeID, manager.dockerReapProtectedSlugs())
}
