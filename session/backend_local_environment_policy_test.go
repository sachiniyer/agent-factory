package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// configuredSessionEnvPassthrough and SessionEnvPassthroughForInstance are the two
// merge points that build a session's default-deny pass-through allowlist from an
// explicit instance grant plus the resolved config. Both once discarded the error
// from sessionenv.NormalizeExtraNames with `normalized, _ :=`. Because that helper
// returns (nil, err) on the FIRST invalid name, one bad entry — delivered by any
// ingress that does not pre-validate names (an off-box agent-server's policy file
// is JSON-shape-checked only) — silently blanked the entire merged allowlist,
// including every valid entry, with a nil error. The agent then ran its panes and
// hooks with no operator-granted credential names, an undiagnosable fail-closed
// misconfiguration. Every other ingress (config.toml parse, `af config set`, the
// --session-env instance flag, tmux SetEnvPassthrough) fails loudly on an invalid
// name; these tests pin that the two merge points now do the same.

// instanceResolvingPassthrough builds an instance whose project-config resolution
// returns rc.SessionEnvPassthrough verbatim, the way a batched restore's
// RestoreContext answers from memory. It is the hermetic injection seam every
// instance-scoped resolver already reads, so the merge sees names the policy wire
// delivered without driving git on the host.
func instanceResolvingPassthrough(t *testing.T, passthrough []string) *Instance {
	t.Helper()
	work := t.TempDir()
	inst := &Instance{Title: "passthrough-merge", Path: work}
	rc := NewRestoreContext()
	rc.repoFromPath = func(path string) (*config.RepoContext, error) {
		return &config.RepoContext{Root: path, IdentityRoot: path, ID: "id-passthrough-merge"}, nil
	}
	rc.resolveConfig = func(*config.RepoContext) (*config.ResolvedConfig, error) {
		resolved := &config.ResolvedConfig{}
		resolved.Config.SessionEnvPassthrough = passthrough
		return resolved, nil
	}
	inst.restoreCtx.Store(rc)
	return inst
}

// The off-box agent-server trigger: an agent policy's session_env_passthrough is
// JSON-validated for shape but not for POSIX-name semantics, so a hand-edited
// NAME=value entry reaches the merge through config.LoadConfig (which short-
// circuits to the installed policy). Before the fix the error was discarded and
// the whole list went empty silently; now it must fail loudly, naming the
// position and never the value.
func TestConfiguredSessionEnvPassthroughFailsLoudlyOnInvalidPolicyNames(t *testing.T) {
	t.Cleanup(config.ClearAgentPolicy)
	policy := config.AgentPolicy{
		Version: config.AgentPolicyVersion,
		Global:  config.PolicyLayer{SessionEnvPassthrough: []string{"VALID_API_KEY", "FOO=bar", "ANOTHER_VALID_NAME"}},
	}
	config.InstallAgentPolicy(policy)

	names, err := configuredSessionEnvPassthrough(nil)
	require.Error(t, err, "an invalid policy name must fail loudly, not silently blank the list")
	require.Nil(t, names, "the invalid merge must return no list, not the blanked allowlist")
	require.ErrorContains(t, err, "invalid session environment pass-through")
	require.ErrorContains(t, err, "position 2", "the error names the offending position")
	require.NotContains(t, err.Error(), "FOO=bar",
		"the invalid value must not be rendered: a NAME=value entry is the credential this boundary protects")
}

// A valid-only policy merges with the explicit grant, dedupes, and sorts — the
// no-regression control for the loud-failure test above. A duplicate across the
// explicit and policy halves collapses to one entry.
func TestConfiguredSessionEnvPassthroughMergesAndNormalizesValidPolicyNames(t *testing.T) {
	t.Cleanup(config.ClearAgentPolicy)
	policy := config.AgentPolicy{
		Version: config.AgentPolicyVersion,
		Global:  config.PolicyLayer{SessionEnvPassthrough: []string{"MY_KEY", "EXTRA_KEY"}},
	}
	config.InstallAgentPolicy(policy)

	names, err := configuredSessionEnvPassthrough([]string{"EXPLICIT_KEY", "MY_KEY"})
	require.NoError(t, err)
	assert.Equal(t, []string{"EXPLICIT_KEY", "EXTRA_KEY", "MY_KEY"}, names,
		"explicit + policy names are merged, deduped, and sorted")
}

// Without a policy, configuredSessionEnvPassthrough answers the explicit grant
// normalized on its own — the legacy behaviour the fix must preserve.
func TestConfiguredSessionEnvPassthroughNormalizesExplicitOnlyWithoutPolicy(t *testing.T) {
	t.Cleanup(config.ClearAgentPolicy)

	names, err := configuredSessionEnvPassthrough([]string{"Z_KEY", "A_KEY", "A_KEY"})
	require.NoError(t, err)
	assert.Equal(t, []string{"A_KEY", "Z_KEY"}, names, "explicit names are deduped and sorted")
}

// The instance-scoped merge (the pane/tab/worktree/editor population) fails
// loudly on an invalid name delivered through the resolved config, rather than
// answering an empty list with a nil error.
func TestSessionEnvPassthroughForInstanceFailsLoudlyOnInvalidNames(t *testing.T) {
	inst := instanceResolvingPassthrough(t, []string{"VALID_API_KEY", "FOO=bar"})

	names, err := SessionEnvPassthroughForInstance(inst)
	require.Error(t, err, "an invalid resolved name must fail loudly, not silently blank the list")
	require.Nil(t, names, "the invalid merge must return no list, not the blanked allowlist")
	require.ErrorContains(t, err, "invalid session environment pass-through")
	require.ErrorContains(t, err, "position 2")
	require.NotContains(t, err.Error(), "FOO=bar",
		"the invalid value must not be rendered (credential non-disclosure)")
}

// An invalid explicit grant alone (with a valid resolved config) also fails
// loudly: the merge validates the union, not just the config half.
func TestSessionEnvPassthroughForInstanceFailsLoudlyOnInvalidExplicitGrant(t *testing.T) {
	inst := instanceResolvingPassthrough(t, []string{"VALID_API_KEY"})
	inst.mu.Lock()
	inst.sessionEnvPassthrough = []string{"BAD=name"}
	inst.mu.Unlock()

	_, err := SessionEnvPassthroughForInstance(inst)
	require.Error(t, err)
	require.ErrorContains(t, err, "invalid session environment pass-through")
}

// Valid explicit + valid resolved names merge, dedupe, and sort — the
// no-regression control for the instance-scoped loud-failure test.
func TestSessionEnvPassthroughForInstanceMergesValidNames(t *testing.T) {
	inst := instanceResolvingPassthrough(t, []string{"MY_KEY", "EXTRA_KEY"})
	inst.mu.Lock()
	inst.sessionEnvPassthrough = []string{"EXPLICIT_KEY", "MY_KEY"}
	inst.mu.Unlock()

	names, err := SessionEnvPassthroughForInstance(inst)
	require.NoError(t, err)
	assert.Equal(t, []string{"EXPLICIT_KEY", "EXTRA_KEY", "MY_KEY"}, names,
		"explicit + resolved names are merged, deduped, and sorted")
}

// The freeze a prepared plan records is the answer admission already resolved, so
// it bypasses the merge (and its validation) and is returned verbatim. An invalid
// name in the freeze is a plan-construction defect, not a merge input; the freeze
// path must keep returning it without re-validating, exactly as before the fix.
func TestSessionEnvPassthroughForInstanceFreezeBypassesMerge(t *testing.T) {
	inst := instanceResolvingPassthrough(t, []string{"FOO=bar"})
	release := inst.freezeEnvPassthrough([]string{"FROZEN_NAME"})
	defer release()

	names, err := SessionEnvPassthroughForInstance(inst)
	require.NoError(t, err)
	assert.Equal(t, []string{"FROZEN_NAME"}, names,
		"the freeze is the plan's resolved answer, returned without re-reading or re-validating")
}
