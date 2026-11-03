package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// SweepStaleSetupPayloads removes <AF home>/af-setup/<id> staging dirs whose
// session id is not in keep — payloads (often secrets) that an exit dropped
// the session's last handle without reclaiming (#6674 review): ghost-record
// kills, creates that failed inside NewInstance after the hook ran, daemons
// that died mid-path. The settled-kill path already reclaims its own, so
// this is the catch-all for every exit that has no instance left to ask.
//
// keep must name EVERY session id the caller can still reach — live,
// archived, tombstoned and pending-create rows alike. A caller whose session
// view is known-incomplete must not invoke this at all: the daemon defers
// the sweep until its skipped-repos set drains, the same err-toward-sparing
// posture the orphan-container sweep takes. And each name is re-validated as
// a payload-dir name (the same single-element rule discardLocalSetupPayload
// applies), so a removal can only ever target a direct child of af-setup —
// never a path the guard would have refused to compute.
//
// Directories only, and best-effort throughout: a leftover is scratch space,
// not a correctness hazard, so enumeration and removal failures only log.
func SweepStaleSetupPayloads(keep map[string]bool) {
	home, err := config.GetConfigDir()
	if err != nil {
		log.WarningLog.Printf("setup payload sweep: cannot resolve the AF home; skipping: %v", err)
		return
	}
	root := filepath.Join(home, setupStagingRootName)
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.WarningLog.Printf("setup payload sweep: cannot list %s: %v", root, err)
		}
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir, ok := localSetupPayloadSessionDir(entry.Name())
		if !ok {
			log.WarningLog.Printf("setup payload sweep: skipping %q — not a payload-dir name", entry.Name())
			continue
		}
		if keep[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			log.WarningLog.Printf("setup payload sweep: could not remove %s: %v", dir, err)
			continue
		}
		log.InfoLog.Printf("setup payload sweep: removed stale staging dir %s", dir)
	}
}
