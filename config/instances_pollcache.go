package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/sachiniyer/agent-factory/log"
)

// RepoFileSignature is the write-safe change signature of one repo's
// instances.json (#5169). It captures every property of the file object a
// write can move without touching the byte stream the cache never sees:
//
//   - (dev, ino) names the file object. Every af writer — SaveRepoInstances,
//     UpdateRepoInstances, the schema-migration write-back — goes through
//     AtomicWriteFile's rename, which always lands a NEW inode, so af's own
//     writes can never reuse the signature they invalidate. That is the
//     "generation bump" an in-process writer would otherwise have to provide.
//   - size and nanosecond mtime catch in-place rewrites; both move unless the
//     writer produces identical-length content within one filesystem timestamp
//     tick.
//   - ctime closes the residual hole mtime leaves: it is not settable, so a
//     back-dated restore (cp -p, tar, rsync -t) of same-size content over the
//     same inode still moves it. On platforms without a known ctime field the
//     helper reports zero and the rest of the signature still stands.
//   - perm catches a chmod that changes whether the file may be read at all,
//     which the daemon must observe rather than serve cached bytes.
//
// A signature hit therefore means the bytes parsed are the bytes on disk; any
// write — atomic rename, in-place rewrite, metadata-only chmod — produces a
// different signature and forces a full re-read, re-migrate, and re-parse.
type RepoFileSignature struct {
	dev     uint64
	ino     uint64
	size    int64
	mtimeNS int64
	ctimeNS int64
	perm    uint32
	known   bool
}

// Known reports whether the signature was computed from a real stat. A zero
// signature never compares equal to a real one, so callers may hand it around
// without a separate validity flag.
func (s RepoFileSignature) Known() bool { return s.known }

// statRepoFileSignature stats path and returns its signature, or the zero
// signature if the stat fails or the platform has no Stat_t to read. Failing
// open — unknown rather than a partial guess — means a file this cannot
// fingerprint is simply never cached and always re-read.
func statRepoFileSignature(path string) RepoFileSignature {
	info, err := os.Stat(path)
	if err != nil {
		return RepoFileSignature{}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return RepoFileSignature{}
	}
	return RepoFileSignature{
		dev:     uint64(st.Dev),
		ino:     uint64(st.Ino),
		size:    info.Size(),
		mtimeNS: info.ModTime().UnixNano(),
		ctimeNS: statCTimeNS(st),
		perm:    uint32(info.Mode().Perm()),
		known:   true,
	}
}

// RepoInstancesPollResult is one pass over every repo's instances.json: the
// same triple LoadAllRepoInstancesReportingMissing returns, plus a signature
// for every repo whose file was observed this call. Only signature-bearing
// repos may be cached downstream; a repo whose file was missing, unreadable
// under the stat, or rewritten mid-parse gets no signature, so nothing downstream
// keys a cache to a state that cannot be re-verified next tick.
type RepoInstancesPollResult struct {
	Instances  map[string]json.RawMessage
	Skipped    []RepoInstancesSkip
	Missing    map[string]bool
	Signatures map[string]RepoFileSignature
}

type repoInstancesPollEntry struct {
	sig RepoFileSignature
	raw json.RawMessage
}

// RepoInstancesFileCache is the per-repo file cache behind the daemon's
// instance poll (#5169). It fuses what used to be two full passes over every
// repo file — MigrateAllRepoInstancesForDaemonLoad's migration sweep and
// LoadAllRepoInstancesReportingMissing's read+extract — into one stat-gated
// load: a file whose signature still matches the cached entry is served from
// the cache without being read, validated, normalized, or migrated at all.
//
// The cached value is the extracted instances array, exactly what
// loadRepoInstancesForAll returned: normalized array bytes for a loadable file,
// the raw file bytes for a corrupt one (the daemon's own decoder then raises
// the corruption and reports it, as before), or "[]" for a missing/empty file.
// Keeping the bytes means a downstream consumer that missed its own cache can
// still parse a hit — and means a corrupt file keeps costing nothing per tick.
type RepoInstancesFileCache struct {
	mu      sync.Mutex
	entries map[string]repoInstancesPollEntry
	// hits and reads exist so tests and benchmarks can observe that an
	// unchanged poll tick performs zero file reads.
	hits  atomic.Int64
	reads atomic.Int64
}

// NewRepoInstancesFileCache returns an empty cache. One instance is meant to
// live behind the daemon's poll loop; tests construct their own.
func NewRepoInstancesFileCache() *RepoInstancesFileCache {
	return &RepoInstancesFileCache{entries: make(map[string]repoInstancesPollEntry)}
}

// Stats reports cache hits and real file reads since the cache was created.
func (c *RepoInstancesFileCache) Stats() (hits, reads int64) {
	return c.hits.Load(), c.reads.Load()
}

// LoadAll returns the per-repo view the daemon refreshes from. Its contract
// mirrors LoadAllRepoInstancesReportingMissing — same Instances map, same
// Missing semantics, same skip-list shape for repoIDs that cannot resolve to a
// path — with the migration sweep's error behavior fused in: a file that exists
// but cannot be read, or fails to migrate for a reason other than malformed
// content, aborts the whole load the way the standalone sweep did, while a
// malformed file keeps producing its raw bytes for the daemon to name.
//
// Each poll that observes a file's signature unchanged returns the cached
// extraction without touching the file, so a steady-state tick is
// O(number of repo directories), not O(total records across them).
func (c *RepoInstancesFileCache) LoadAll() (RepoInstancesPollResult, error) {
	dir, err := instancesDirPath()
	if err != nil {
		return RepoInstancesPollResult{}, err
	}
	result := RepoInstancesPollResult{
		Instances:  make(map[string]json.RawMessage),
		Missing:    make(map[string]bool),
		Signatures: make(map[string]RepoFileSignature),
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return RepoInstancesPollResult{}, fmt.Errorf("failed to read instances directory: %w", err)
	}

	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoID := entry.Name()
		path, perr := repoInstancesPath(repoID)
		if perr != nil {
			// The standalone sweeps answered this case twice over: the
			// migrator's walk warned and skipped the name, and the loader's
			// repoInstancesPath failure logged it again and landed it on the
			// skip list. Keep all three outputs.
			log.WarningLog.Printf("skipping instances subdirectory %q: not a valid repo id: %v", repoID, perr)
			log.WarningLog.Printf("failed to load instances for repo %s: %v", repoID, perr)
			result.Skipped = append(result.Skipped, RepoInstancesSkip{RepoID: repoID, Err: perr})
			continue
		}
		seen[path] = true
		raw, missing, sig, err := c.loadRepoFile(repoID, path)
		if err != nil {
			return RepoInstancesPollResult{}, err
		}
		if missing {
			result.Missing[repoID] = true
		}
		result.Instances[repoID] = raw
		if sig.Known() {
			result.Signatures[repoID] = sig
		}
	}
	c.prune(seen)
	return result, nil
}

// loadRepoFile produces the loader-visible bytes for one repo file: a cache
// hit serves the recorded extraction, a miss performs today's full pipeline —
// read, migrate-if-needed, extract — and caches its result under the file's
// observed signature.
func (c *RepoInstancesFileCache) loadRepoFile(repoID, path string) (json.RawMessage, bool, RepoFileSignature, error) {
	pre := statRepoFileSignature(path)
	if pre.Known() {
		if ent, ok := c.get(path); ok && ent.sig == pre {
			c.hits.Add(1)
			return ent.raw, false, pre, nil
		}
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return json.RawMessage("[]"), true, RepoFileSignature{}, nil
		}
		// The standalone migration sweep read every repo file BEFORE the
		// loader ran, so a read failure never reached the loader's skip list —
		// it aborted the whole refresh. This read stands in for that one, so
		// its failure aborts the same way.
		return nil, false, RepoFileSignature{}, fmt.Errorf("failed to migrate instances for repo %s: %w",
			repoID, fmt.Errorf("failed to read repo instances: %w", rerr))
	}
	c.reads.Add(1)
	if len(bytes.TrimSpace(data)) == 0 {
		empty := json.RawMessage("[]")
		return empty, false, c.store(path, pre, empty), nil
	}
	if !ProveJSONSchemaVersion(data, InstancesSchemaVersion) {
		return c.migrateAndExtract(repoID, path, data, pre)
	}
	instances, err := instancesArrayInCurrentEnvelope(data)
	if err != nil {
		// Preserve the decode-and-report contract for corrupt content: the
		// all-repo loaders always returned the file's own bytes so the
		// daemon's row decode is what names the corruption.
		raw := json.RawMessage(data)
		return raw, false, c.store(path, pre, raw), nil
	}
	return instances, false, c.store(path, pre, instances), nil
}

// migrateAndExtract is the cold path for bytes ProveJSONSchemaVersion did not
// prove current: legacy schemas, newer schemas, and corrupt content all meet
// the real migration machinery here, under the file lock, exactly as the
// standalone sweep ran it. The plan's Validate is swapped for the same
// array-capturing form extractInstancesArray uses, so the envelope still decodes
// exactly once (#3726).
func (c *RepoInstancesFileCache) migrateAndExtract(repoID, path string, data []byte, pre RepoFileSignature) (json.RawMessage, bool, RepoFileSignature, error) {
	var captured json.RawMessage
	validated := false
	plan := NewInstancesSchemaMigrationPlan(path)
	plan.Validate = func(migrated []byte) error {
		validated = true
		var err error
		captured, err = decodeInstancesEnvelope(migrated)
		return err
	}
	_, result, merr := LoadAndMigrateSchemaFile(plan)
	if merr != nil {
		var newer *UnsupportedSchemaVersionError
		switch {
		case errors.As(merr, &newer):
			return nil, false, RepoFileSignature{}, fmt.Errorf("failed to migrate instances for repo %s: %w", repoID, merr)
		case errors.Is(merr, errInstancesSchemaContent):
			// A malformed file is skip-and-warn per repo, not an abort: hand
			// the daemon the raw file bytes, whose row decode reports the
			// corruption exactly as before.
			raw := json.RawMessage(data)
			return raw, false, c.store(path, pre, raw), nil
		default:
			return nil, false, RepoFileSignature{}, fmt.Errorf("failed to migrate instances for repo %s: %w", repoID, merr)
		}
	}
	if !validated {
		return nil, false, RepoFileSignature{}, fmt.Errorf("failed to migrate instances for repo %s: %w",
			repoID, fmt.Errorf("%w: instances envelope was migrated without being decoded", errInstancesSchemaContent))
	}
	if result.Migrated {
		// The file was just rewritten; the pre-read signature describes the old
		// inode. Report no signature so the next poll re-observes the migrated
		// file, then reaches steady state.
		return captured, false, RepoFileSignature{}, nil
	}
	// The probe refused a file the full plan proved current — escaped keys or
	// unusual formatting — and nothing was rewritten, so the stat bracket still
	// describes the bytes that were parsed.
	return captured, false, c.store(path, pre, captured), nil
}

// store records raw for path under pre, and returns the signature to report —
// but only when the file still carries that signature AFTER the parse. The
// bracket is what makes the cache correct: if the parse read stale bytes and a
// write landed mid-flight, the post-parse stat disagrees with pre, nothing is
// cached, and the next poll re-reads rather than serving a signature-attested
// view of superseded content.
func (c *RepoInstancesFileCache) store(path string, pre RepoFileSignature, raw json.RawMessage) RepoFileSignature {
	if !pre.Known() {
		return RepoFileSignature{}
	}
	post := statRepoFileSignature(path)
	if post != pre {
		return RepoFileSignature{}
	}
	c.mu.Lock()
	c.entries[path] = repoInstancesPollEntry{sig: pre, raw: raw}
	c.mu.Unlock()
	return pre
}

func (c *RepoInstancesFileCache) get(path string) (repoInstancesPollEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.entries[path]
	return ent, ok
}

// prune drops entries for paths this poll did not enumerate — repo dirs that
// vanished, or paths from an AF home this cache is no longer pointed at — so
// the map stays bounded by the live repo set.
func (c *RepoInstancesFileCache) prune(seen map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for path := range c.entries {
		if !seen[path] {
			delete(c.entries, path)
		}
	}
}
