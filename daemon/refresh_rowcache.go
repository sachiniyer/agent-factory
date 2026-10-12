package daemon

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// repoRowsOutcome is everything refreshDaemonInstances derived from one repo's
// instances.json the last time the file carried its current signature: the
// parse verdict — empty, corrupt, or the decoded rows — plus each row's
// precomputed daemon key. While the signature holds, a poll replays this
// outcome instead of re-reading, re-validating, and re-decoding the file
// (#5169).
//
// Only the PARSE is replayed. The materialize loop still runs over the cached
// rows against the live existing map on every tick, so existing-wins pointer
// reuse, per-tick retry of a row that cannot materialize, the legacy-ID
// backfill write, ghost task-run counting, and the reread retraction accounting
// all run through the same code they did before — fed cached rows instead of a
// fresh unmarshal of unchanged bytes.
type repoRowsOutcome struct {
	sig config.RepoFileSignature
	// corruptErr is set for a file whose rows could not be decoded at all; the
	// poll replays the corrupt-repo branch, warning and all.
	corruptErr error
	// empty is set for a file whose rows decoded to nothing — a "[]", a "null",
	// or the same via a missing file, though missing files never carry a
	// signature and so never reach this.
	empty bool
	rows  []session.InstanceData
	keys  []string
}

// repoRowsOutcomeCache stores one outcome per repoID, keyed by the file
// signature that produced it. Entries are only ever installed when the loader
// reported a signature — proof the file state was observed — and a hit requires
// the signature to still match, so a write of any kind re-derives the outcome.
type repoRowsOutcomeCache struct {
	mu      sync.Mutex
	entries map[string]repoRowsOutcome
	hits    atomic.Int64
}

// refreshRowOutcomes is the poll loop's outcome cache. Package-level because
// refreshDaemonInstances is a free function shared by restoreInstances and
// refreshLocked; entries are signature-gated, so a test that reuses a repoID in
// a different AF home can only miss.
var refreshRowOutcomes = &repoRowsOutcomeCache{entries: make(map[string]repoRowsOutcome)}

// get returns the cached outcome for repoID when the file's signature is
// unchanged. A repo the loader did not sign — a missing file, a stat failure,
// or a file the parse saw get rewritten mid-flight — never hits.
func (c *repoRowsOutcomeCache) get(repoID string, sig config.RepoFileSignature) (repoRowsOutcome, bool) {
	if !sig.Known() {
		return repoRowsOutcome{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.entries[repoID]
	if !ok || ent.sig != sig {
		return repoRowsOutcome{}, false
	}
	c.hits.Add(1)
	return ent, true
}

func (c *repoRowsOutcomeCache) store(repoID string, outcome repoRowsOutcome) {
	if !outcome.sig.Known() {
		return
	}
	c.mu.Lock()
	c.entries[repoID] = outcome
	c.mu.Unlock()
}

// prune drops outcomes for repos absent from this poll's load result — dirs
// that vanished or were skipped — keeping the map bounded by the live set.
func (c *repoRowsOutcomeCache) prune(all map[string]json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for repoID := range c.entries {
		if _, ok := all[repoID]; !ok {
			delete(c.entries, repoID)
		}
	}
}

// hitsTotal exposes the hit count for tests asserting that an unchanged tick
// replayed rather than re-parsed.
func (c *repoRowsOutcomeCache) hitsTotal() int64 { return c.hits.Load() }

// recordCorruptedRepo is the corrupted-file branch of the refresh loop, lifted
// out so a replayed corrupt outcome and a fresh parse failure share one body:
// warn, mark the repo skipped, and re-hydrate its prior in-memory instances so
// a transient corruption does not silently drop running sessions (#603).
func recordCorruptedRepo(repoID string, err error, existing, next map[string]*session.Instance, skipped []SkippedRepo) []SkippedRepo {
	log.WarningLog.Printf("daemon skipping repo %s: corrupted instances.json: %v", repoID, err)
	skipped = append(skipped, SkippedRepo{RepoID: repoID, Reason: SkippedRepoReasonCorruptedInstancesJSON})
	if existing != nil {
		keyPrefix := repoID + "\x00"
		for key, inst := range existing {
			if strings.HasPrefix(key, keyPrefix) {
				next[key] = inst
			}
		}
	}
	return skipped
}
