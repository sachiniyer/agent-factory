package commands

import (
	"bytes"
	"testing"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestProjectSetAppliedLiveKeyNoticeIsNotRestart pins the effect notice that
// `af config set --project <key>` prints for a project-overridable key whose
// project override is re-read per relevant operation. Such a key is
// EffectAppliedLive: the daemon resolves it from disk on each session create
// (default_program / program_overrides / default_accounts via
// config.ResolveConfigForRepo) or on each archive (on_archive_command), so the
// change reaches the very next operation with NO restart. The honest notice
// therefore names the key's actual effect surface instead of telling the
// operator to restart af and the daemon.
//
// Each applied-live row asserts two clauses:
//  1. stdout does NOT contain the restart sentence ("restart them to apply") —
//     this is the bug: the always-true SetResult.RequiresRestart used to route
//     every project key through projectConfigRestartNotice.
//  2. stdout DOES mention the key's real effect surface ("session" for the three
//     per-session-create keys, "archive" for on_archive_command) — this catches a
//     fix that silently drops the notice AND a fix that uses session-scoped
//     wording for the per-archive key.
//
// The keep_restart subtable pins the opposite partition — root_agent.program and
// branch_prefix are EffectNextDaemonStart, so they MUST still print the restart
// notice — so a fix that over-corrects by dropping the notice for a restart-needed
// key fails.
func TestProjectSetAppliedLiveKeyNoticeIsNotRestart(t *testing.T) {
	appliedLive := []struct {
		name       string
		key        string
		value      string
		wantSubstr string
	}{
		{"default_program", "default_program", "codex", "session"},
		{"program_overrides.claude", "program_overrides.claude", "claude", "session"},
		{"default_accounts.codex", "default_accounts.codex", "work", "session"},
		{"on_archive_command", "on_archive_command", "echo done", "archive"},
	}
	for _, c := range appliedLive {
		t.Run(c.name, func(t *testing.T) {
			projectSetNoticeRun(t, c.key, c.value, func(t *testing.T, got string) {
				require.NotContains(t, got, "restart them to apply",
					"`af config set --project %s` printed a restart notice, but %s is "+
						"EffectAppliedLive and needs no restart. Got: %q",
					c.key, c.key, got)
				require.Contains(t, got, c.wantSubstr,
					"`af config set --project %s` gave no guidance naming its effect "+
						"surface for an EffectAppliedLive key. Expected the notice to "+
						"mention %q (the key's actual effect surface), but it was absent. "+
						"Got: %q",
					c.key, c.wantSubstr, got)
			})
		})
	}

	t.Run("keep_restart", func(t *testing.T) {
		restartNeeded := []struct {
			name, key, value string
		}{
			{"root_agent.program", "root_agent.program", "claude"},
			{"branch_prefix", "branch_prefix", "af-"},
		}
		for _, c := range restartNeeded {
			t.Run(c.name, func(t *testing.T) {
				projectSetNoticeRun(t, c.key, c.value, func(t *testing.T, got string) {
					require.Contains(t, got, "restart them to apply",
						"`af config set --project %s` (%s is EffectNextDaemonStart) must "+
							"still print the restart notice. Got: %q",
						c.key, c.key, got)
				})
			})
		}
	})
}

// projectSetNoticeRun drives the --project branch of `af config set` for one key
// in a freshly registered project (configSetProjectFlag=".") and hands the
// captured stdout to the caller's assertions. Each subtest gets its own temp
// home, git repo, and registered project so writes cannot leak between rows.
func projectSetNoticeRun(t *testing.T, key, value string, assert func(*testing.T, string)) {
	t.Helper()

	_, repo := setupConfigExplainCommandTest(t, "schema_version = 1\n")
	t.Setenv("AF_DAEMON_URL", "")
	_, err := config.RegisterProject(repo)
	require.NoError(t, err)
	t.Chdir(repo)

	oldProject := configSetProjectFlag
	configSetProjectFlag = "."
	t.Cleanup(func() { configSetProjectFlag = oldProject })

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, configSetCmd.RunE(cmd, []string{key, value}),
		"set --project %s failed unexpectedly", key)

	assert(t, out.String())
}
