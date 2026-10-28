package doctor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// TestStaleTempHomeSparesSymlinkedActiveHome guards the data-loss bug in
// checkStaleTempHomes where AGENT_FACTORY_HOME is a symlink to a
// differently-named directory under the temp dir. The active-home guard
// computed activeHome with filepath.Clean (no symlink resolution), so the
// sweep's real-target candidate did not compare equal and the populated active
// home was offered for removal — and, with --fix, deleted by os.RemoveAll.
//
// The fix resolves activeHome, tempDir, and each candidate through normalizeHome
// (filepath.EvalSymlinks), matching the sibling checkLeakedDaemonBinaries.
//
// The fixture models a USED home left alone past MinTempHomeAge, the
// abandoned-but-populated state af doctor --fix is designed to clean up:
//   - the home carries the afHomeMarkers (state.json, instances/, an aged
//     agent-factory.log) so isAFHome passes;
//   - log.Initialize(false) runs before Run, in the CLI order
//     (commands/doctorcmd.go:118). Opening a pre-existing log with O_APPEND
//     does not update its mtime, so the age gate passes for a used home;
//   - the lock probe is stubbed to AnswerNo (takeable), modelling a dead
//     daemon — the positive proof the check requires before authorising a
//     removal.
//
// A home that meets every one of those preconditions must STILL be spared,
// because it is THIS run's active home. The symlink spelling must not defeat
// that recognition. (Over-sparing is already guarded by the existing
// TestTakeableLockTempHomeRemovedOnFix, which removes a non-active abandoned
// home under the same conditions.)
func TestStaleTempHomeSparesSymlinkedActiveHome(t *testing.T) {
	tempRoot := t.TempDir()
	realDir := filepath.Join(tempRoot, "af-real")
	require.NoError(t, os.MkdirAll(realDir, 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(realDir, "instances"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "state.json"), []byte(`{"schema_version":1}`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "config.json"), []byte(`{}`), 0644))
	// Pre-existing aged log: O_APPEND open in log.Initialize leaves the mtime
	// untouched, so the age gate passes and the bug's preconditions hold. A
	// fresh log would be created with mtime=now and spare the home, modelling a
	// home af never used — not the abandoned-but-populated case here.
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "agent-factory.log"), []byte("old\n"), 0644))
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{
		filepath.Join(realDir, "state.json"),
		filepath.Join(realDir, "config.json"),
		filepath.Join(realDir, "agent-factory.log"),
		filepath.Join(realDir, "instances"),
		realDir,
	} {
		require.NoError(t, os.Chtimes(p, old, old))
	}
	// AGENT_FACTORY_HOME is a symlink to a DIFFERENTLY-NAMED sibling under the
	// same temp root. The sweep's entry.IsDir() filters symlinks, so only the
	// real-target directory (/.../af-real) becomes a candidate; the link spelling
	// (/.../af-link) never does. That is the spelling mismatch the guard must
	// see through.
	linkPath := filepath.Join(tempRoot, "af-link")
	require.NoError(t, os.Symlink(realDir, linkPath))
	t.Setenv("AGENT_FACTORY_HOME", linkPath)
	// Dead daemon: the lock is takeable, the positive proof that — without the
	// active-home guard — authorises os.RemoveAll.
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer { return daemon.AnswerNo() })
	opts := macLikeTempHomeOptions(t, tempRoot, true)
	opts.ConfigDir = linkPath
	// CLI order: initialize logging before running doctor (commands/doctorcmd.go:118).
	log.Initialize(false)
	defer log.Close()

	report, err := Run(opts)
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "stale-temp-home"),
		"the symlinked active home must be recognised as active and spared, not offered for removal")
	require.DirExists(t, realDir, "the populated active home must not be deleted by --fix")
}
