package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The sweep reclaims af-setup/<id> staging dirs whose session id has no
// remaining handle — the exits discardLocalSetupPayload cannot reach because
// no instance survives to ask it (ghost-record kills, creates that failed
// after the hook staged, daemons that died mid-path). Anything the caller
// still names stays; so does anything that is not a payload-dir name.
func TestSweepStaleSetupPayloads(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	root := filepath.Join(home, "af-setup")
	mk := func(name string) string {
		d := filepath.Join(root, name, "payload")
		require.NoError(t, os.MkdirAll(d, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(d, "secret"), []byte("x"), 0600))
		return filepath.Join(root, name)
	}
	live := mk("sess-live")
	archived := mk("sess-archived")
	ghost := mk("sess-ghost")          // no record anywhere — reclaimed
	failed := mk("sess-failed-create") // hook staged, create dropped — reclaimed
	// A non-directory entry and a name that is not a payload-dir name are
	// left alone, not swept.
	require.NoError(t, os.WriteFile(filepath.Join(root, "loose-file"), []byte("x"), 0644))

	SweepStaleSetupPayloads(map[string]bool{"sess-live": true, "sess-archived": true})

	for _, d := range []string{live, archived} {
		_, err := os.Stat(d)
		assert.NoError(t, err, "%s is still reachable — its payload must stay", d)
	}
	for _, d := range []string{ghost, failed} {
		_, err := os.Stat(d)
		assert.True(t, os.IsNotExist(err), "%s has no remaining handle — its payload must be reclaimed", d)
	}
	_, err = os.Stat(filepath.Join(root, "loose-file"))
	assert.NoError(t, err, "a non-directory entry under af-setup is not a staging dir — leave it")
}

// No af-setup root at all — the common case on a home that never ran a
// setup hook — is a silent no-op, not a warning.
func TestSweepStaleSetupPayloadsNoRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	SweepStaleSetupPayloads(map[string]bool{"sess": true})
}

// The keep set is the caller's completeness claim: an empty map removes
// every payload dir present — which is why the daemon only invokes this
// after its skipped-repos set drains (a partial view would delete payloads
// live sessions can still consume).
func TestSweepStaleSetupPayloadsEmptyKeepSetRemovesAll(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	orphan := filepath.Join(home, "af-setup", "sess-orphan", "payload")
	require.NoError(t, os.MkdirAll(orphan, 0755))

	SweepStaleSetupPayloads(map[string]bool{})
	_, err = os.Stat(filepath.Dir(orphan))
	assert.True(t, os.IsNotExist(err), "an empty keep set sweeps every payload dir")
}
