package commands

import (
	"bytes"
	"os"
	"strings"
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
// config.KeyEffectClass map the set side already consulted, so branch_prefix and
// on_archive_command (EffectAppliedLive, per-archive) fell into the generic
// "sessions created ..." notice — the wrong surface for the latter, and at the
// time the wrong timing for the former, which was EffectNextDaemonStart until
// #4539 made each create resolve branch_prefix from the live config plus the
// project's override. PR #4381 fixed the set side; this pins the unset side so
// set and unset cannot re-diverge.
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
		{"branch_prefix", "branch_prefix", `branch_prefix = "af-"` + "\n", "sessions created in this project"},
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

	// The mirror, stated as EQUALITY rather than as two lists of substrings.
	// branch_prefix is the key that moved between the partitions above (#4539 made
	// it EffectAppliedLive), and it is exactly the case where "neither verb says
	// restart" would still pass while the two printed different live sentences —
	// which is the divergence this whole test exists to prevent.
	t.Run("branch_prefix_unset_and_set_print_the_same_notice", func(t *testing.T) {
		var unsetNotice, setNotice string
		projectUnsetNoticeRun(t, "branch_prefix", "branch_prefix = \"af-\"\n", func(_ *testing.T, got string) {
			unsetNotice = lastNoticeLine(got)
		})
		projectSetNoticeRun(t, "branch_prefix", "af-", func(_ *testing.T, got string) {
			setNotice = lastNoticeLine(got)
		})
		require.Equal(t, setNotice, unsetNotice,
			"`af config unset --project branch_prefix` and `af config set --project branch_prefix` "+
				"must print the SAME effect notice; a user who clears an override and one who writes "+
				"it are told about the same key.")
		require.Contains(t, unsetNotice, "sessions created in this project",
			"branch_prefix is resolved per create since #4539, so both verbs must name the "+
				"session-create surface. Got: %q", unsetNotice)
		require.NotContains(t, unsetNotice, "restart them to apply",
			"branch_prefix needs no restart since #4539. Got: %q", unsetNotice)
	})
}

// lastNoticeLine is the effect notice: the final non-empty line the verb printed,
// after the line naming what it wrote and where.
func lastNoticeLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
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
