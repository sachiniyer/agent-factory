package commands

import (
	"bytes"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestProjectUnsetNoticeMirrorsSet pins the unset-side mirror of the set-side
// notice branches. `af config unset --project <key>` must print the same effect
// notice `af config set --project <key>` does, so the two verbs never disagree
// about when a cleared override takes effect.
//
// This guards the footgun that produced the bug: PR #4088 added a divergent
// rootAgentConfigKey prefix check on the unset side instead of the authoritative
// config.KeyEffectClass map the set side already consulted, so branch_prefix
// (EffectNextDaemonStart) and on_archive_command (EffectAppliedLive, per-archive)
// fell into the generic "sessions created ..." notice — wrong timing for the
// former, wrong surface for the latter. PR #4381 fixed the set side; this pins
// the unset side so set and unset cannot re-diverge.
//
// root_agent / root_agent.* on unset are covered separately by
// TestRootAgentDottedProjectUnsetNamesAdoptedSessionRemedy (which pins the
// restart notice and the root-agent adoption remedy), so they are not duplicated
// here; this test covers the two keys #4381 explicitly deferred plus one
// session-scoped representative to guard the else branch against over-correction.
func TestProjectUnsetNoticeMirrorsSet(t *testing.T) {
	// default_program guards the generic session-scoped else branch (a fix that
	// routed every key through the restart notice would fail this); on_archive_command
	// is the archive-scoped special case #4381 deferred to the unset side.
	appliedLive := []struct {
		name       string
		key        string
		seed       string
		wantSubstr string
	}{
		{"default_program", "default_program", `default_program = "codex"` + "\n", "session"},
		{"on_archive_command", "on_archive_command", `on_archive_command = "echo done"` + "\n", "archive operations"},
	}
	for _, c := range appliedLive {
		t.Run(c.name, func(t *testing.T) {
			projectUnsetNoticeRun(t, c.key, c.seed, func(t *testing.T, got string) {
				require.NotContains(t, got, "restart them to apply",
					"`af config unset --project %s` printed a restart notice, but %s is "+
						"EffectAppliedLive and needs no restart. Got: %q",
					c.key, c.key, got)
				require.Contains(t, got, c.wantSubstr,
					"`af config unset --project %s` gave no guidance naming its effect "+
						"surface for an EffectAppliedLive key. Expected the notice to "+
						"mention %q (the key's actual effect surface), but it was absent. "+
						"Got: %q",
					c.key, c.wantSubstr, got)
			})
		})
	}

	// branch_prefix is EffectNextDaemonStart (read from the frozen startup config),
	// so unsetting it must print the restart notice, not the session-scoped sentence.
	// This is the wrong-timing half of #4381's deferred unset-side follow-up.
	t.Run("branch_prefix_must_restart", func(t *testing.T) {
		projectUnsetNoticeRun(t, "branch_prefix", "branch_prefix = \"af-\"\n", func(t *testing.T, got string) {
			require.Contains(t, got, "restart them to apply",
				"`af config unset --project branch_prefix` (EffectNextDaemonStart) must "+
					"print the restart notice like `af config set --project` does. Got: %q", got)
			require.NotContains(t, got, "sessions created",
				"`af config unset --project branch_prefix` must not print the "+
					"session-scoped sentence. Got: %q", got)
		})
	})
}

// projectUnsetNoticeRun drives the --project branch of `af config unset` for one
// key in a freshly registered project (configUnsetProjectFlag=".") whose personal
// config already seeds an override for key, and hands the captured stdout to the
// caller's assertions. Each subtest gets its own temp home, git repo, and
// registered project so writes cannot leak between rows.
func projectUnsetNoticeRun(t *testing.T, key, seed string, assert func(*testing.T, string)) {
	t.Helper()

	_, repo := setupConfigExplainCommandTest(t, "schema_version = 1\n")
	t.Setenv("AF_DAEMON_URL", "")
	project, err := config.RegisterProject(repo)
	require.NoError(t, err)
	path, err := config.ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(seed), 0644))
	t.Chdir(repo)

	oldProject := configUnsetProjectFlag
	configUnsetProjectFlag = "."
	t.Cleanup(func() { configUnsetProjectFlag = oldProject })

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, configUnsetCmd.RunE(cmd, []string{key}),
		"unset --project %s failed unexpectedly", key)

	assert(t, out.String())
}
