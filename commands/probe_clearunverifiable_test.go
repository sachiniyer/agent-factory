package commands

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
	"github.com/stretchr/testify/require"
)

func writeUnverifiableStagedArtifact(t *testing.T, dir, id, homeDir string, age time.Duration) {
	t.Helper()
	previous := filepath.Join(dir, ".af.af-upgrade-"+id+".previous")
	require.NoError(t, os.WriteFile(previous, []byte("preserved"), 0o755))
	stamp := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(previous, stamp, stamp))

	ownerPath := filepath.Join(dir, ".af.af-upgrade-"+id+".owner")
	owner := []byte(`{"schema_version":1,"transaction_id":"` + id + `","home_dir":"` + homeDir + `","staged_at":"` + time.Now().UTC().Add(-age).Format(time.RFC3339Nano) + `"}` + "\n")
	require.NoError(t, config.AtomicWriteFile(ownerPath, owner, 0o600))
	require.NoError(t, os.Chtimes(ownerPath, stamp, stamp))
}

// probeTarget builds an executable alongside an ArtifactUnverifiable staged
// artifact (a stale .previous with an owner record pointing at a home af
// cannot stat), and stubs the executable seam at it. The artifact is aged past
// stagedArtifactGrace so the youth rule cannot promote it to ArtifactLive. It
// returns the artifact's .previous path so a test can assert the probe left it
// on disk (the probe must be read-only).
func probeTarget(t *testing.T, id string) (target, previous string) {
	t.Helper()
	dir := t.TempDir()
	target = filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))
	previous = filepath.Join(dir, ".af.af-upgrade-"+id+".previous")
	writeUnverifiableStagedArtifact(t, dir, id, "/nonexistent/agent-factory-home", 2*time.Hour)
	originalExec := osExecutableFn
	osExecutableFn = func() (string, error) { return target, nil }
	t.Cleanup(func() { osExecutableFn = originalExec })
	return target, previous
}

// The probe honours upgrade_clear_unverifiable_artifacts=true and does NOT
// block on an ArtifactUnverifiable staged artifact, mirroring the locked swap's
// policy. Before the fix it passed ArtifactScanOptions{} so ClearUnverifiable
// was always false and the config was never consulted.
func TestProbe_ClearUnverifiableConfigHonoured(t *testing.T) {
	home := upgradeHome(t)
	require.NoError(t, config.AtomicWriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("upgrade_clear_unverifiable_artifacts = true\n"),
		0o600,
	))

	const id = "upgrade-foreign"
	target, previous := probeTarget(t, id)

	// The fix: the probe reads the config and does not suppress the launch-time
	// install for an unverifiable artifact the operator has opted into clearing.
	active := upgradeOwningThisExecutable()
	require.Nil(t, active,
		"with upgrade_clear_unverifiable_artifacts=true the probe must not block on an unverifiable artifact")

	// Sanity-check the divergence the fix closes: the same artifact scanned with
	// ClearUnverifiable false (what the probe passed before the fix) does block,
	// and scanned with ClearUnverifiable true (what the locked swap passes) does
	// not. The whole divergence is the one ClearUnverifiable field.
	require.NotNil(t,
		foreignUpgradeStagingOver(target, "", upgradetxn.ArtifactScanOptions{ClearUnverifiable: false}),
		"with ClearUnverifiable false an unverifiable artifact must block")
	require.Nil(t,
		foreignUpgradeStagingOver(target, "", upgradetxn.ArtifactScanOptions{ClearUnverifiable: true}),
		"with ClearUnverifiable true an unverifiable artifact must not block")

	// The probe must remain READ-ONLY: it runs before the locks, so it must not
	// set the artifact aside even when the config is on. Clear stays false, and
	// clearable() returns false when Clear is false regardless of
	// ClearUnverifiable. The artifact stays exactly where the scan found it,
	// for the locked swap to sweep.
	require.FileExists(t, previous, "the probe must never rename a staged artifact; it runs before the locks")
	aside, err := filepath.Glob(previous + ".debris-*")
	require.NoError(t, err)
	require.Empty(t, aside, "the probe set an artifact aside without holding the lock; that is the race the locks exist for")
}

// With the config off (the default, conservative answer), the probe still
// blocks on an ArtifactUnverifiable artifact — the fix does not change the
// default-policy path, only consults the config the operator may have set.
func TestProbe_ClearUnverifiableDefaultStillBlocks(t *testing.T) {
	upgradeHome(t) // no config.toml → default false

	const id = "upgrade-foreign"
	probeTarget(t, id)

	require.NotNil(t, upgradeOwningThisExecutable(),
		"with the config unset the probe must still block on an unverifiable artifact — an unreadable claim is still a claim")
}

// A live transaction's artifact is never unblocked by the config. The probe
// honours the config only for ArtifactUnverifiable; ArtifactLive blocks
// unconditionally (blocks() returns true regardless of ClearUnverifiable).
func TestProbe_LiveArtifactStillBlocksWithConfigOn(t *testing.T) {
	home := upgradeHome(t)
	require.NoError(t, config.AtomicWriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("upgrade_clear_unverifiable_artifacts = true\n"),
		0o600,
	))

	dir := t.TempDir()
	target := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))
	// A young artifact with no owner record is ArtifactLive by the youth rule
	// (staged moments ago, well inside stagedArtifactGrace).
	previous := filepath.Join(dir, ".af.af-upgrade-upgrade-live.previous")
	require.NoError(t, os.WriteFile(previous, []byte("preserved"), 0o755))

	originalExec := osExecutableFn
	osExecutableFn = func() (string, error) { return target, nil }
	t.Cleanup(func() { osExecutableFn = originalExec })

	require.NotNil(t, upgradeOwningThisExecutable(),
		"an artifact staged moments ago must still block the probe even with upgrade_clear_unverifiable_artifacts=true")
}
