package apiclient

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv(envDaemonURL, "")
	t.Setenv(envDaemonToken, "")
	oldU, oldT := FlagDaemonURL, FlagDaemonToken
	FlagDaemonURL, FlagDaemonToken = "", ""
	t.Cleanup(func() { FlagDaemonURL, FlagDaemonToken = oldU, oldT })
}

func TestSavedTargetPrecedenceFlagEnvSaved(t *testing.T) {
	isolate(t)
	require.False(t, IsRemoteTarget())
	require.Equal(t, TargetLocal, ResolveTargetInfo().Source)

	require.NoError(t, SaveTarget(SavedTarget{URL: "http://saved:8443", Token: "saved-tok"}))
	info := ResolveTargetInfo()
	require.Equal(t, "http://saved:8443", info.URL)
	require.Equal(t, TargetSaved, info.Source)
	require.True(t, info.HasToken)
	require.True(t, IsRemoteTarget())

	t.Setenv(envDaemonURL, "http://env:1")
	info = ResolveTargetInfo()
	require.Equal(t, "http://env:1", info.URL)
	require.Equal(t, TargetEnv, info.Source)

	FlagDaemonURL = "http://flag:2"
	info = ResolveTargetInfo()
	require.Equal(t, "http://flag:2", info.URL)
	require.Equal(t, TargetFlag, info.Source)
}

// A saved token must never ride along to a host named some other way.
func TestSavedTokenIsNeverPairedWithAFlagOrEnvURL(t *testing.T) {
	isolate(t)
	require.NoError(t, SaveTarget(SavedTarget{URL: "http://saved:8443", Token: "saved-tok"}))

	FlagDaemonURL = "http://other:1"
	_, tok := resolveTarget()
	require.Empty(t, tok, "the saved token must not be sent to a flag-named host")

	FlagDaemonURL = ""
	t.Setenv(envDaemonURL, "http://other:1")
	_, tok = resolveTarget()
	require.Empty(t, tok, "nor to an env-named host")

	t.Setenv(envDaemonURL, "")
	t.Setenv(envDaemonToken, "rotated")
	u, tok := resolveTarget()
	require.Equal(t, "http://saved:8443", u)
	require.Equal(t, "rotated", tok, "an explicit token overrides the saved one for the saved url")
}

func TestHasDaemonTokenIgnoresTheSavedOperatorToken(t *testing.T) {
	isolate(t)
	require.NoError(t, SaveTarget(SavedTarget{URL: "http://saved:8443", Token: "saved-tok"}))
	require.False(t, HasDaemonToken(), "a saved operator token is not a sandbox identity")
}

func TestSavedTargetFileIs0600(t *testing.T) {
	isolate(t)
	require.NoError(t, SaveTarget(SavedTarget{URL: "http://saved:8443", Token: "t"}))
	path, err := SavedTargetPath()
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// Fail closed: a damaged saved target must not turn into "use the local daemon".
func TestDamagedSavedTargetFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(path string){
		"loose mode": func(p string) { require.NoError(t, os.Chmod(p, 0o644)) },
		"malformed":  func(p string) { require.NoError(t, os.WriteFile(p, []byte("{nope"), 0o600)) },
		"bad url":    func(p string) { require.NoError(t, os.WriteFile(p, []byte(`{"url":"ftp://x"}`), 0o600)) },
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			require.NoError(t, SaveTarget(SavedTarget{URL: "http://saved:8443", Token: "t"}))
			path, _ := SavedTargetPath()
			mutate(path)
			require.True(t, IsRemoteTarget(), "must not read as local")
			_, err := NewTargeted()
			require.Error(t, err)
			require.True(t, strings.Contains(err.Error(), "af target clear") || strings.Contains(err.Error(), "chmod") || strings.Contains(err.Error(), "af target set"))
		})
	}
}

func TestSaveTargetRejectsInvalidURLAndClearIsIdempotent(t *testing.T) {
	isolate(t)
	require.Error(t, SaveTarget(SavedTarget{URL: "not a url"}))
	require.NoError(t, ClearSavedTarget())
	require.NoError(t, SaveTarget(SavedTarget{URL: "http://saved:8443"}))
	require.NoError(t, ClearSavedTarget())
	require.False(t, IsRemoteTarget())
}
