package commands

import (
	"bytes"
	"testing"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestProjectSetHumanOutputSurfacesWriterWarnings pins the fix for the
// `af config set --project` human/text gap. The project writer
// (SetProjectConfigValue) appends write-time validation warnings to
// SetResult.Warnings — most importantly the "no <agent> account named X is
// registered" warning for default_accounts.<agent> — and the human output must
// surface them on stderr, exactly as the global branch of the same command
// already does and as the --json branch already carries them in the envelope.
//
// Before the fix this branch printed the "set …" echo and the effect notice but
// never iterated res.Warnings, so the writer-produced warning was silently
// dropped on the human surface (while --json still serialized it — which is why
// no existing test caught the asymmetry: the lone project-branch test captured
// only stdout and asserted only the effect-notice wording).
func TestProjectSetHumanOutputSurfacesWriterWarnings(t *testing.T) {
	t.Run("unregistered_default_account_warns_on_stderr", func(t *testing.T) {
		stdout, stderr := projectSetCaptureStreamsRun(t, "default_accounts.codex", "work")

		require.Contains(t, stderr, `no codex account named "work" is registered on this machine`,
			"the --project branch must surface the writer's unregistered-account warning on "+
				"stderr, mirroring the global branch of the same command")
		require.Contains(t, stderr, "af accounts add codex work",
			"the warning must name the remediation command that registers the account")
		require.NotContains(t, stderr, "applies to sessions created in this project",
			"the effect notice is a stdout surface; it must never be duplicated on stderr")

		require.NotContains(t, stdout, "is registered on this machine",
			"the writer warning is a stderr-only surface; it must not also appear on stdout")
		require.Contains(t, stdout, "set default_accounts.codex = work for project",
			"the set echo line remains on stdout")
		require.Contains(t, stdout, "applies to sessions created in this project",
			"the effect notice remains on stdout")
		// The echo precedes the effect notice on stdout, matching the global
		// branch's "what the value MEANS matters more than when it takes effect"
		// ordering.
		require.Less(t,
			bytes.Index([]byte(stdout), []byte("set default_accounts.codex = work")),
			bytes.Index([]byte(stdout), []byte("applies to sessions created in this project")),
			"the echo line must precede the effect notice on stdout")
	})

	t.Run("no_writer_warning_leaves_stderr_empty", func(t *testing.T) {
		// Control: a key that produces no writer warning (default_program is not
		// a default_accounts.<agent> key, so defaultAccountWriteWarning returns "")
		// must leave stderr empty, proving the loop prints only what the writer
		// produced and never a spurious line.
		stdout, stderr := projectSetCaptureStreamsRun(t, "default_program", "codex")
		require.Empty(t, stderr,
			"a key with no writer warning must leave stderr empty, got: %q", stderr)
		require.Contains(t, stdout, "set default_program = codex for project",
			"the set echo line remains on stdout for the no-warning case")
	})
}

// projectSetCaptureStreamsRun drives the --project branch of `af config set`
// for one key in a freshly registered project (configSetProjectFlag=".") and
// hands back BOTH captured streams. project_set_notice_repro_test.go's helper
// captures only stdout and discards stderr entirely — which is exactly the gap
// that let this bug ship — so this helper exists to assert the stderr surface.
// Each subtest gets its own temp home, git repo, and registered project so
// writes cannot leak between rows.
func projectSetCaptureStreamsRun(t *testing.T, key, value string) (stdout, stderr string) {
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
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	require.NoError(t, configSetCmd.RunE(cmd, []string{key, value}),
		"set --project %s failed unexpectedly", key)
	return out.String(), errOut.String()
}
