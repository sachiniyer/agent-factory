package api

import (
	"strings"
	"testing"
)

func TestProposalRemoteSessionRefusesAnUnscopedRepo(t *testing.T) {
	setupRepoForCmd(t)
	t.Setenv("TMUX", "")
	t.Setenv("AF_DAEMON_URL", "http://127.0.0.1:1")
	proposalSessionFlag = "me"
	t.Cleanup(func() { proposalSessionFlag = "" })
	_, methods := stubProposalDaemon(t)
	for _, repo := range []string{"app", "~/app"} {
		repoFlag = repo
		_, err := runCmdCaptureStdout(t, proposalChangesCmd, nil)
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("--repo %q with --session: error = %v, want a must-be-absolute error", repo, err)
		}
	}
	if len(*methods) != 0 {
		t.Fatalf("no route may be called, got %v", *methods)
	}
}
