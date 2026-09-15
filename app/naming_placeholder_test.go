package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordPlaceholderProvision captures every InstanceOptions the backend factory
// is asked to provision, and hands back a LocalBackend so the flow continues.
// The factory is the boundary #2599 is about: everything past it is a real
// runtime being established (docker run, a clone, launch_cmd), so what the
// naming flow puts INTO it is the whole question.
func recordPlaceholderProvision(t *testing.T) *[]session.InstanceOptions {
	t.Helper()
	var seen []session.InstanceOptions
	t.Cleanup(session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		seen = append(seen, opts)
		return &session.LocalBackend{}, nil
	}))
	return &seen
}

// TestStartNewInstanceNeverProvisionsTheRepoBackend is the #2599 regression
// guard. Pressing `n` builds a placeholder row so the rail has something to type
// a title into — and that went through session.NewInstance, which RESOLVES the
// create's backend and provisions it. In a repo whose config declares
// backend = "docker"/"ssh"/"hook" that is a real provision before the user has
// typed anything: dockerRuntime.Provision runs `docker run` + a clone + an
// agent-server, hookRuntime.Provision runs the repo's launch_cmd.
//
// The observable damage was that the provision could REFUSE, and then the naming
// form never opened at all — such a repo could not create a session from the TUI
// by any keystroke. So this asserts both halves: the placeholder is never asked
// to provision the repo's runtime, and the form opens regardless of what the repo
// declares.
//
// A unit test cannot see the provision itself (a real dockerRuntime would need a
// daemon and a container), which is why scripts/tui-2599-scenario.sh drives the
// real TUI against the real factory. This pins the input that decides it.
func TestStartNewInstanceNeverProvisionsTheRepoBackend(t *testing.T) {
	for _, repoBackend := range []string{"", "docker", "ssh", "hook"} {
		name := repoBackend
		if name == "" {
			name = "local default"
		}
		t.Run(name, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(200, 1)
			seen := recordPlaceholderProvision(t)
			h.repoRoot = repoDeclaringBackend(t, repoBackend)

			model, _ := h.startNewInstance()

			require.Same(t, h, model)
			requireNamingFormOpened(t, h, "pressing n must open the form, not report an error")
			assert.Equal(t, stateNew, h.state,
				"the naming form must open no matter which backend the repo declares")
			require.NotNil(t, h.namingInstance, "the rail needs a row to name")

			require.Len(t, *seen, 1, "exactly one placeholder is built")
			got := (*seen)[0]
			assert.Equal(t, session.BackendLocal, got.Backend,
				"the placeholder must pin the local runtime; anything else provisions a sandbox for a row")
			assert.False(t, got.ForceRemote,
				"and must not reach the hook runtime through the legacy selector either")
		})
	}
}

// TestNamingPlaceholderIgnoresAnUnreadableRepoConfig keeps the pin honest at the
// edge that used to hard-fail. A repo whose `backend` key names nothing
// resolvable made session.NewInstance return that error, so `n` was refused
// before the form opened. The placeholder no longer resolves the repo's key at
// all, so the form opens and the daemon states the verdict at submit — where the
// user can still see the name they typed.
func TestNamingPlaceholderIgnoresAnUnreadableRepoConfig(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	recordPlaceholderProvision(t)
	h.repoRoot = repoDeclaringBackend(t, "moonbase")

	model, _ := h.startNewInstance()

	require.Same(t, h, model)
	requireNamingFormOpened(t, h, "an unresolvable repo backend must not refuse the keypress")
	assert.Equal(t, stateNew, h.state, "the naming form must still open")
	assert.Empty(t, h.errBox.FullError())
}

// TestNamingPlaceholderDoesNotReachDockerRuntime is the sharpest statement of
// the bug, and the only one that does not depend on the factory seam: with the
// REAL factory in place, a repo declaring backend = "docker" with no docker.image
// used to fail inside dockerRuntime.Provision — the error quoted in #2599 — and
// the form never opened. Nothing here stubs the runtime, so a regression that
// restores the repo-config resolution fails on the real provisioner's refusal.
func TestNamingPlaceholderDoesNotReachDockerRuntime(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	repoRoot := repoDeclaringBackend(t, "docker")
	// No docker.image: exactly the config from the issue's repro.
	h.repoRoot = repoRoot

	model, _ := h.startNewInstance()

	require.Same(t, h, model)
	requireNamingFormOpened(t, h, "the docker runtime must never be asked to provision a naming row")
	assert.Equal(t, stateNew, h.state)
	assert.NotContains(t, h.errBox.FullError(), "docker.image",
		"a BackendConfigError here means the provisioner ran for a placeholder")
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, session.WorkspaceLocalWorktree, h.namingInstance.Capabilities().Workspace,
		"the placeholder row is a row, not a sandbox")
}

// writeRepoDockerImage is a small helper for the config shape #2194 documents,
// kept next to the tests that need a resolvable docker repo.
func writeRepoDockerImage(t *testing.T, repoRoot, image string) {
	t.Helper()
	cfgDir := filepath.Join(repoRoot, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	body := "backend = \"docker\"\n\n[docker]\nimage = \"" + image + "\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, config.TomlConfigFileName), []byte(body), 0o644))
}

// TestNamingPlaceholderSkipsAResolvableDockerRepo covers the second half of
// #2599 — the half the issue inferred rather than observed. Where the docker
// preconditions PASS nothing refuses the create, so the provision SUCCEEDS: a
// container came up at naming time, named from Slugify("") because no title had
// been typed, and the daemon then provisioned the real session on submit — one
// create, two sandboxes, with the orphan reaped only if cancel got there.
//
// With the real factory and a repo that would satisfy the config check, the
// placeholder must still be local: no container, one create, one sandbox.
func TestNamingPlaceholderSkipsAResolvableDockerRepo(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	repoRoot := repoDeclaringBackend(t, "")
	writeRepoDockerImage(t, repoRoot, "alpine:3.20")
	h.repoRoot = repoRoot

	model, _ := h.startNewInstance()

	require.Same(t, h, model)
	requireNamingFormOpened(t, h)
	require.Equal(t, stateNew, h.state)
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, session.WorkspaceLocalWorktree, h.namingInstance.Capabilities().Workspace,
		"a satisfiable docker config must not be provisioned for a naming row either")
}
