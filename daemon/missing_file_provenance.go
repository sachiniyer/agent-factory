package daemon

import "sync"

// missingFileProvenanceSet tracks the set of repos the daemon OBSERVED as missing
// on a prior poll, so the preservation loop keeps protecting their live in-memory
// sessions after the file is recreated as an empty "[]" (e.g. by a preserved
// session's targeted write) — without also preserving a repo that was LEGITIMATELY
// emptied (another process killed the last session, or an administrator replaced
// the roster with "[]"). A present-but-empty file is preserved only when the
// daemon previously saw it missing; a genuinely empty file the daemon never
// observed as missing is the authoritative state and its stale in-memory sessions
// are dropped. An entry is held only while the file stays missing or
// present-but-empty; once the file carries real rows again the repo recovers and
// the entry is dropped. Package-level because refreshDaemonInstances is a free
// function shared by restoreInstances and refreshLocked; entries are repo-scoped,
// so a test that reuses a repoID in a different AF home can only miss (and the
// reset helper clears it between tests).
type missingFileProvenanceSet struct {
	mu    sync.Mutex
	repos map[string]bool
}

// preservedMissingFileRepos is the singleton instance of the observed-missing
// provenance set above.
var preservedMissingFileRepos = &missingFileProvenanceSet{repos: make(map[string]bool)}

// resetPreservedMissingFileReposForTest clears the provenance set. Tests that
// stage a present-but-empty file without a prior missing-file observation must not
// inherit provenance from an earlier test that used the same repoID, so a
// missing-file episode in one test never bleeds into the legitimate-empty
// assertion of another.
func resetPreservedMissingFileReposForTest() {
	preservedMissingFileRepos.mu.Lock()
	preservedMissingFileRepos.repos = make(map[string]bool)
	preservedMissingFileRepos.mu.Unlock()
}
