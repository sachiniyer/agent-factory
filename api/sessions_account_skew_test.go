package api

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session"
)

// TestSessionsCreate_AccountSkewSuggestsAPasteableKill is #3420.
//
// The version-skew error names a session that now exists and must be removed, and
// the whole value of naming it is that the command it prints can be pasted. A
// session title may contain a space or a shell metacharacter — title validation
// rejects only whitespace-only titles and control characters — so interpolating
// it raw produced `af sessions kill my session` (fails: too many arguments) or, in
// the metacharacter case, a command that parses into something else entirely.
//
// Quoting alone is not the whole answer: `--name=-worker` is a valid title, and
// `af sessions kill '-worker'` still exits "unknown shorthand flag", so the
// suggestion must terminate options too — the convention
// daemon/sandbox_preserve.go's kill suggestion states.
//
// This path is deliberately reachable: a newer CLI sends --account to an older
// daemon, which drops the field it does not know. It is printed exactly when
// someone is already cleaning up after a create they did not want.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the message reads `af sessions kill my
// session with spaces` — unquoted, and unusable as printed.
func TestSessionsCreate_AccountSkewSuggestsAPasteableKill(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
		want  string
	}{
		{
			name:  "spaces",
			title: "my session with spaces",
			want:  "af sessions kill -- 'my session with spaces'",
		},
		{
			name:  "shell metacharacters",
			title: "test;echo pwned",
			want:  "af sessions kill -- 'test;echo pwned'",
		},
		{
			// A leading dash is a valid title and quoting cannot save it: only the
			// option terminator can. This is the case Codex raised on #3429.
			name:  "leading dash",
			title: "-worker",
			want:  "af sessions kill -- -worker",
		},
		{
			// A title needing no quoting still prints clean, which is the reason this
			// goes through shellsuggest rather than an always-quote helper: the string
			// exists to be read as much as pasted.
			name:  "already shell-safe",
			title: "captain",
			want:  "af sessions kill -- captain",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			silenceStdio(t)

			repo := t.TempDir()
			require.NoError(t, exec.Command("git", "init", repo).Run())

			// The account has to exist, or the pre-create validation refuses before
			// the daemon is ever called and this error path is unreachable.
			cfgHome, err := config.GetConfigDir()
			require.NoError(t, err)
			_, err = agentaccount.Register(cfgHome, "claude", "work")
			require.NoError(t, err)

			// The skew itself: an older daemon accepts the create and silently drops
			// the account field, so what comes back is scoped to nobody.
			prevCreate := createSessionViaDaemon
			createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
				return &session.InstanceData{Title: req.Title, Account: ""}, nil
			}
			t.Cleanup(func() { createSessionViaDaemon = prevCreate })

			setSessionsCreateFlags(t, tc.title, repo, false, false)
			prevProgram, prevAccount := createProgramFlag, createAccountFlag
			createProgramFlag, createAccountFlag = "claude", "work"
			t.Cleanup(func() { createProgramFlag, createAccountFlag = prevProgram, prevAccount })

			err = sessionsCreateCmd.RunE(sessionsCreateCmd, nil)
			require.Error(t, err, "an unapplied --account must be reported as a failure")
			msg := err.Error()

			assert.Contains(t, msg, "`"+tc.want+"`",
				"the version-skew error must print a kill command a user can paste:\n  full message: %s", msg)
			// Not just "contains the right string somewhere": the raw title must not
			// also appear as a bare command, which is how a %s-formatted suggestion
			// would pass a substring check for a safe title while still being wrong.
			if tc.title != "captain" && tc.title != "-worker" {
				assert.NotContains(t, msg, "`af sessions kill "+tc.title+"`",
					"the unquoted command is still in the message:\n  full message: %s", msg)
			}
			// The message must still name the session and the account it failed to
			// apply — the quoting fix must not cost the diagnosis.
			assert.Contains(t, msg, tc.title, "the error no longer names the session")
			assert.Contains(t, msg, `"work"`, "the error no longer names the account it did not apply")
		})
	}
}
