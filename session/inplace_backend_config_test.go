package session

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// remoteBackendKinds are the three non-local runtimes. Each has no local
// worktree at all, so each contradicts an in-place create.
var remoteBackendKinds = []BackendKind{BackendDocker, BackendSSH, BackendHook}

// TestNewInstance_InPlaceRefusesExplicitRemoteBackend pins the half of the
// contradiction check that already worked: an explicit --backend naming a
// non-local runtime, and the legacy ForceRemote hook selector.
func TestNewInstance_InPlaceRefusesExplicitRemoteBackend(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := initTempGitRepo(t)

	for _, kind := range remoteBackendKinds {
		t.Run(string(kind), func(t *testing.T) {
			_, err := NewInstance(InstanceOptions{
				Title:   "s",
				Path:    repo,
				Program: "claude",
				Backend: kind,
				InPlace: true,
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInPlaceRemoteBackend)
		})
	}

	t.Run("force remote", func(t *testing.T) {
		_, err := NewInstance(InstanceOptions{
			Title:       "s",
			Path:        repo,
			Program:     "claude",
			ForceRemote: true,
			InPlace:     true,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInPlaceRemoteBackend)
	})
}

// TestNewInstance_InPlaceRefusesRepoConfiguredRemoteBackend is the #2778
// regression test.
//
// The in-place contradiction was judged on opts.Backend alone, but an EMPTY
// opts.Backend does not mean local — it means "resolve from the repo's `backend`
// config key". So a repo declaring backend = "docker"/"ssh"/"hook" sailed past
// the check with InPlace still set, and the create then either failed with an
// unrelated provisioning-config error (nothing about in-place) or, in a fully
// configured repo, SUCCEEDED: a session recorded as attached to the user's own
// working tree whose agent actually runs in a sandbox that cannot see it.
//
// The assertion is on the error's identity rather than merely on "an error
// happened": the pre-fix path errors too, which is exactly why the bug survived.
func TestNewInstance_InPlaceRefusesRepoConfiguredRemoteBackend(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	for _, kind := range remoteBackendKinds {
		t.Run(string(kind), func(t *testing.T) {
			repo := initTempGitRepo(t)
			writeInRepoConfig(t, repo, map[string]any{"backend": string(kind)})

			_, err := NewInstance(InstanceOptions{
				Title:   "s",
				Path:    repo,
				Program: "claude",
				InPlace: true,
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInPlaceRemoteBackend,
				"an in-place create against a repo-configured %s backend must be refused as the contradiction it is, not left to fail later on provisioning config", kind)
			assert.Contains(t, err.Error(), string(kind),
				"the refusal must name the backend that was selected, or the user cannot tell which config key to change")
		})
	}
}

// TestNewInstance_InPlaceStillAllowedOnLocal is the non-regression half: the
// resolved-backend check must not refuse the ordinary in-place create. Three
// shapes reach the local runtime — no config at all, an explicit local backend,
// and a repo declaring backend = "local".
func TestNewInstance_InPlaceStillAllowedOnLocal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cases := []struct {
		name    string
		config  map[string]any
		backend BackendKind
	}{
		{name: "no repo config"},
		{name: "explicit local backend", backend: BackendLocal},
		{name: "repo config backend local", config: map[string]any{"backend": "local"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initTempGitRepo(t)
			if tc.config != nil {
				writeInRepoConfig(t, repo, tc.config)
			}
			inst, err := NewInstance(InstanceOptions{
				Title:   "s",
				Path:    repo,
				Program: "claude",
				Backend: tc.backend,
				InPlace: true,
			})
			require.NoError(t, err)
			require.NotNil(t, inst)
			assert.True(t, inst.inPlace, "the in-place selection must survive the check it passed")
		})
	}
}

// TestNewInstance_UnresolvableBackendKeepsItsCanonicalError guards the
// three-valued edge: a `backend` key naming nothing resolvable is neither local
// nor remote, so the in-place check must not convert it into an in-place
// refusal. The user's actual problem is the unparseable value, and the factory
// below already reports it in one place.
func TestNewInstance_UnresolvableBackendKeepsItsCanonicalError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := initTempGitRepo(t)
	writeInRepoConfig(t, repo, map[string]any{"backend": "kubernetes"})

	_, err := NewInstance(InstanceOptions{
		Title:   "s",
		Path:    repo,
		Program: "claude",
		InPlace: true,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInPlaceRemoteBackend)
	assert.Contains(t, err.Error(), "kubernetes",
		fmt.Sprintf("the unparseable backend value must be what the error names, got %v", err))
}
