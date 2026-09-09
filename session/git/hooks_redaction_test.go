package git

import (
	"strings"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
)

func TestPostWorktreeHookStartMessageRedactsCredentialBeforeQuoting(t *testing.T) {
	const secret = "S3NT1NELVALUEDONOTLOG"
	command := `api_key="` + secret + `"`
	message := postWorktreeHookStartMessage("/tmp/worktree", "/tmp/hook.log", command)
	got := aflog.RedactCredentials(message)

	if strings.Contains(got, secret) {
		t.Fatalf("Go-quoted hook command leaked its credential through the log sink: %s", got)
	}
	if want := `running post-worktree hook in /tmp/worktree (output: /tmp/hook.log): "api_key=\"[redacted-secret]\""`; got != want {
		t.Errorf("redacted log message = %q, want %q", got, want)
	}
}
