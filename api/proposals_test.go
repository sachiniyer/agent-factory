package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/proposal"
	"github.com/sachiniyer/agent-factory/session"
)

const proposalTestSessionID = "33333333-3333-4333-8333-333333333333"

func stubProposalSelf(t *testing.T) {
	t.Helper()
	root := repoFlag
	t.Setenv("TMUX", "/tmp/tmux-test/default,1,0")
	stubCurrentTmuxName(t, func() (string, error) { return "af_me_agent", nil })
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{
			ID:       proposalTestSessionID,
			Title:    "me",
			TmuxName: "af_me_agent",
			Worktree: session.GitWorktreeData{RepoPath: root},
		}}, nil
	})
}

func TestProposalChangesOutsideSessionErrors(t *testing.T) {
	setupRepoForCmd(t)
	t.Setenv("TMUX", "")
	stubCurrentTmuxName(t, func() (string, error) {
		return "", errors.New("not running inside a tmux session")
	})
	_, err := runCmdCaptureStdout(t, proposalChangesCmd, nil)
	want := "af proposal changes needs a session: run it inside an af session, or pass --session <title>"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestProposalRefusesDaemonURL(t *testing.T) {
	setupRepoForCmd(t)
	t.Setenv("AF_DAEMON_URL", "http://127.0.0.1:1")
	_, err := runCmdCaptureStdout(t, proposalListCmd, nil)
	if !errors.Is(err, errProposalRemote) {
		t.Fatalf("error = %v, want %v", err, errProposalRemote)
	}
}

func TestProposalStartAttachesSelfAndPrintsRules(t *testing.T) {
	setupRepoForCmd(t)
	stubProposalSelf(t)

	out, err := runCmdCaptureStdout(t, proposalStartCmd, []string{"auth-rework"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var got proposalInfo
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON (%q): %v", out, err)
	}
	if got.Slug != "auth-rework" || len(got.Docs) != len(proposal.BuiltinTemplates) {
		t.Fatalf("start output = %+v", got)
	}
	if len(got.AttachedSessions) != 1 {
		t.Fatalf("attached_sessions = %v, want the calling session", got.AttachedSessions)
	}
	if !strings.Contains(got.Rules, got.Path) || !strings.Contains(got.Rules, "af proposal checkpoint") {
		t.Fatalf("rules do not name the path and checkpoint:\n%s", got.Rules)
	}

	if err := os.WriteFile(filepath.Join(got.Path, "goals.md"), []byte("user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runCmdCaptureStdout(t, proposalChangesCmd, nil)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	var changes proposal.Changes
	if err := json.Unmarshal(out, &changes); err != nil {
		t.Fatalf("changes output is not JSON (%q): %v", out, err)
	}
	if len(changes.Edits) != 1 || changes.Edits[0].Author != proposal.AuthorExternal {
		t.Fatalf("changes = %+v, want one external edit", changes)
	}

	out, err = runCmdCaptureStdout(t, proposalListCmd, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list []proposalInfo
	if err := json.Unmarshal(out, &list); err != nil || len(list) != 1 || list[0].LastEdit == "" {
		t.Fatalf("list = %s, %v", out, err)
	}

	if _, err := runCmdCaptureStdout(t, proposalAttachCmd, []string{"missing"}); err == nil ||
		!strings.Contains(err.Error(), "auth-rework") {
		t.Fatalf("attach of an unknown slug = %v, want the existing slugs listed", err)
	}
}

func TestProposalInsideTmuxSurfacesLookupFailure(t *testing.T) {
	setupRepoForCmd(t)
	t.Setenv("TMUX", "/tmp/tmux-test/default,1,0")
	stubCurrentTmuxName(t, func() (string, error) {
		return "", errors.New(`AF_SESSION "a" does not match tmux pane session "b"`)
	})
	for _, tc := range []struct {
		cmd  *cobra.Command
		args []string
	}{
		{proposalChangesCmd, nil},
		{proposalWriteCmd, []string{"p", "goals"}},
		{proposalAddDocCmd, []string{"p", "notes"}},
	} {
		_, err := runCmdCaptureStdout(t, tc.cmd, tc.args)
		if err == nil || !strings.Contains(err.Error(), "AF_SESSION") || strings.Contains(err.Error(), "needs a session") {
			t.Errorf("%s: error = %v, want the lookup failure", tc.cmd.Name(), err)
		}
	}
}
