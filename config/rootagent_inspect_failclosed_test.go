package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the #3264 contract for `af config get root_agent --explain`:
// the surface is documented as "the decision the daemon makes at its NEXT
// start", and since #3241/#3247 that decision is FAIL CLOSED when a personal
// config cannot be loaded or the project registry cannot be listed. The
// pre-#3264 explain described the daemon's old fail-open behavior in exactly
// those states — reporting a legacy entry as the winner over a registry it
// could not read, or erroring out where the daemon's verdict is known.

// corruptInspectionRegistry makes ListProjects fail portably (a stray
// non-directory entry in the registry directory) and probe-proves the fixture,
// so the tests cannot silently pass through the ordinary resolution path.
func corruptInspectionRegistry(t *testing.T) {
	t.Helper()
	dir, err := ProjectRegistryDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray"), []byte("not a project record"), 0o644))
	_, err = ListProjects()
	require.Error(t, err, "fixture failed: ListProjects still succeeds on a corrupt registry")
}

// requireFailClosedTrace asserts the shared shape of a fail-closed explain:
// the effective profile is disabled, every present layer is ignored with the
// cause, and no field claims a config origin.
func requireFailClosedTrace(t *testing.T, rv ResolvedValue, wantReason string) {
	t.Helper()
	profile, ok := rv.Value.(RootAgent)
	require.Truef(t, ok, "explain value must stay a RootAgent profile, got %T", rv.Value)
	assert.False(t, profile.Enabled, "a fail-closed verdict must render disabled — absence of proof is not permission")
	present := 0
	for _, c := range rv.Candidates {
		if !c.Present {
			continue
		}
		present++
		assert.Equalf(t, "ignored", c.Result, "present layer %s must be ignored under fail-closed", c.Layer)
		assert.Containsf(t, c.Reason, wantReason, "layer %s must carry the cause", c.Layer)
	}
	require.NotZero(t, present, "fixture must produce at least one present layer or the trace assertions are vacuous")
	assert.Empty(t, rv.Origins, "no config source decided a fail-closed verdict, so no field may claim an origin")
}

// TestRootAgentExplainFailsClosedOnUnlistableRegistry: with the registry
// unlistable and a legacy root_agents entry present, the pre-#3264 explain
// reported the root ENABLED via the legacy layer — the daemon's old fail-open
// story — misdirecting the operator away from the registry corruption.
func TestRootAgentExplainFailsClosedOnUnlistableRegistry(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	globalTOML := "schema_version = 1\n\n[root_agents]\n\"" + repoRoot + "\" = {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte(globalTOML), 0o644))
	corruptInspectionRegistry(t)

	rv, err := ResolveRootAgentForInspection(repoRoot, false)
	require.NoError(t, err)
	requireFailClosedTrace(t, rv, ProjectRegistryDirName)
}

// TestRootAgentExplainFailsClosedOnUnloadablePersonalConfig: the pre-#3264
// explain errored out on an unloadable personal config, even though the
// daemon's next-start decision in that state is fully known (fail closed,
// #3241). The operator running --explain is exactly the person who needs the
// verdict, the cause, and the file named.
func TestRootAgentExplainFailsClosedOnUnloadablePersonalConfig(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	globalTOML := "schema_version = 1\n\n[root_agents]\n\"" + repoRoot + "\" = {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte(globalTOML), 0o644))
	personalPath := writePersonalConfig(t, project.ID, "[root_agent]\nenabled = tru\n")

	rv, err := ResolveRootAgentForInspection(repoRoot, false)
	require.NoError(t, err, "the verdict is known (fail closed), so --explain must explain it rather than error out")
	requireFailClosedTrace(t, rv, personalPath)
}

// TestRootAgentExplainStrictRegistryFailureStillErrors pins the strict-lookup
// carve-out: an explicit --project selector asked about one project, and with
// the registry unlistable that question has no trustworthy answer — erroring
// stays correct there.
func TestRootAgentExplainStrictRegistryFailureStillErrors(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte("schema_version = 1\n"), 0o644))
	corruptInspectionRegistry(t)

	_, err := ResolveRootAgentForInspection(repoRoot, true)
	require.Error(t, err)
}
