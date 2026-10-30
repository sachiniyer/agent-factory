package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/proposal"
	"github.com/sachiniyer/agent-factory/session"
)

const proposalTestSessionID = "33333333-3333-4333-8333-333333333333"

// proposalTestHost is the daemon side of the service for CLI tests.
type proposalTestHost struct {
	repoID  string
	tabs    map[string]string
	prompts []string
	mode    proposal.PushMode
}

func (h *proposalTestHost) Session(id string) (proposal.Session, error) {
	if id != proposalTestSessionID {
		return proposal.Session{}, errors.New("session not found")
	}
	return proposal.Session{ID: id, Title: "me", RepoID: h.repoID}, nil
}

func (h *proposalTestHost) SessionTitles(string) map[string]string {
	return map[string]string{proposalTestSessionID: "me"}
}

func (h *proposalTestHost) EnsureDocsTab(sessionID, slug string) (string, error) {
	h.tabs[slug] = "docs"
	return "docs", nil
}

func (h *proposalTestHost) CloseDocsTabs(sessionID, slug string) (string, error) {
	name := h.tabs[slug]
	delete(h.tabs, slug)
	return name, nil
}

func (h *proposalTestHost) Publish(proposal.Change) {}

func (h *proposalTestHost) SendPrompt(sessionID, prompt string) (proposal.Delivery, error) {
	h.prompts = append(h.prompts, prompt)
	return proposal.DeliveryConfirmed, nil
}

func (h *proposalTestHost) PublishPush(proposal.PushStatus) {}

func decodeInto[T any](raw []byte) (T, error) {
	var v T
	err := json.Unmarshal(raw, &v)
	return v, err
}

// stubProposalDaemon routes the CLI to an in-process proposal service. Requests
// and replies take a JSON round trip, as they do over HTTP.
func stubProposalDaemon(t *testing.T) (*proposalTestHost, *[]string) {
	t.Helper()
	repo, err := repoFromFlag()
	if err != nil {
		t.Fatal(err)
	}
	host := &proposalTestHost{repoID: repo.ID, tabs: map[string]string{}}
	svc := proposal.NewService(host)
	var methods []string
	pushTarget := func(sessionID string) proposal.PushTarget {
		modes := map[string]proposal.PushMode{}
		for slug := range host.tabs {
			modes[slug] = host.mode
		}
		return proposal.PushTarget{RepoID: host.repoID, SessionID: sessionID, Modes: modes, Ready: true}
	}
	tgt := func(sessionID, repoID, repoPath string) proposal.Target {
		return proposal.Target{SessionID: sessionID, RepoID: repoID, RepoPath: repoPath}
	}
	routes := map[string]func([]byte) (any, error){
		"ListProposals": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.ListProposalsRequest](raw)
			if err != nil {
				return nil, err
			}
			out, err := svc.List(tgt(r.SessionID, r.RepoID, r.RepoPath))
			return daemon.ListProposalsResponse{Proposals: out}, err
		},
		"GetProposal": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.GetProposalRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.Get(tgt(r.SessionID, r.RepoID, r.RepoPath), r.Slug)
		},
		"StartProposal": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.StartProposalRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.Start(r.SessionID, r.Slug, r.Docs)
		},
		"AttachProposal": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.AttachProposalRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.Attach(r.SessionID, r.Slug)
		},
		"DetachProposal": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.DetachProposalRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.Detach(r.SessionID)
		},
		"AddProposalDoc": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.AddProposalDocRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.AddDoc(tgt(r.SessionID, r.RepoID, r.RepoPath), r.Slug, r.Name, proposal.Author(r.Author))
		},
		"RemoveProposalDoc": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.RemoveProposalDocRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.RemoveDoc(tgt(r.SessionID, r.RepoID, r.RepoPath), r.Slug, r.Doc, proposal.Author(r.Author))
		},
		"ReadProposalDoc": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.ReadProposalDocRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.Read(tgt(r.SessionID, r.RepoID, r.RepoPath), r.Slug, r.Doc)
		},
		"WriteProposalDoc": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.WriteProposalDocRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.Write(tgt(r.SessionID, r.RepoID, r.RepoPath), r.Slug, r.Doc, r.Text, proposal.Author(r.Author))
		},
		"PullProposalChanges": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.PullProposalChangesRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.Changes(r.SessionID)
		},
		"CheckpointProposal": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.CheckpointProposalRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.Checkpoint(r.SessionID)
		},
		"SetProposalPushMode": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.SetProposalPushModeRequest](raw)
			if err != nil {
				return nil, err
			}
			host.mode = proposal.PushMode(r.Mode)
			return svc.SetPushMode(pushTarget(r.SessionID), host.mode)
		},
		"SendProposalChanges": func(raw []byte) (any, error) {
			r, err := decodeInto[daemon.SendProposalChangesRequest](raw)
			if err != nil {
				return nil, err
			}
			return svc.SendNow(pushTarget(r.SessionID))
		},
	}
	prev := callProposalRoute
	callProposalRoute = func(method string, req, resp any) error {
		methods = append(methods, method)
		route, ok := routes[method]
		if !ok {
			return fmt.Errorf("unknown route %s", method)
		}
		raw, err := json.Marshal(req)
		if err != nil {
			return err
		}
		out, err := route(raw)
		if err != nil {
			return err
		}
		data, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, resp)
	}
	t.Cleanup(func() { callProposalRoute = prev })
	return host, &methods
}

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

func TestProposalRemoteNeedsRepoOrSession(t *testing.T) {
	setupRepoForCmd(t)
	repoFlag = ""
	t.Setenv("TMUX", "")
	t.Setenv("AF_DAEMON_URL", "http://127.0.0.1:1")
	_, methods := stubProposalDaemon(t)
	_, err := runCmdCaptureStdout(t, proposalListCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--repo is required with --daemon-url") {
		t.Fatalf("error = %v, want the --repo hint", err)
	}
	if len(*methods) != 0 {
		t.Fatalf("no route may be called, got %v", *methods)
	}
}

func TestProposalRemoteSendsRepoPath(t *testing.T) {
	setupRepoForCmd(t)
	t.Setenv("TMUX", "")
	t.Setenv("AF_DAEMON_URL", "http://127.0.0.1:1")
	var got daemon.ListProposalsRequest
	prev := callProposalRoute
	callProposalRoute = func(method string, req, resp any) error {
		got = req.(daemon.ListProposalsRequest)
		return nil
	}
	t.Cleanup(func() { callProposalRoute = prev })
	if _, err := runCmdCaptureStdout(t, proposalListCmd, nil); err != nil {
		t.Fatal(err)
	}
	if got.RepoPath != repoFlag || got.RepoID != "" || got.SessionID != "" {
		t.Fatalf("request = %+v, want repo_path %q as typed", got, repoFlag)
	}
}

func TestProposalRemoteSessionUsesDaemonHostRepo(t *testing.T) {
	setupRepoForCmd(t)
	t.Setenv("TMUX", "")
	t.Setenv("AF_DAEMON_URL", "http://127.0.0.1:1")
	repoFlag = "/srv/remote-only/app"
	proposalSessionFlag = "me"
	t.Cleanup(func() { proposalSessionFlag = "" })
	const otherID = "44444444-4444-4444-8444-444444444444"
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{
			{ID: otherID, Title: "me", Path: "/srv/other", Worktree: session.GitWorktreeData{RepoPath: "/srv/other"}},
			{ID: proposalTestSessionID, Title: "me", Path: "/srv/remote-only/app", Worktree: session.GitWorktreeData{RepoPath: "/srv/remote-only/app"}},
		}, nil
	})
	var got daemon.PullProposalChangesRequest
	prev := callProposalRoute
	callProposalRoute = func(method string, req, resp any) error {
		got = req.(daemon.PullProposalChangesRequest)
		return nil
	}
	t.Cleanup(func() { callProposalRoute = prev })
	if _, err := runCmdCaptureStdout(t, proposalChangesCmd, nil); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != proposalTestSessionID {
		t.Fatalf("session_id = %q, want the session in --repo on the daemon host", got.SessionID)
	}
}

func TestProposalStartAttachesSelfAndPrintsRules(t *testing.T) {
	setupRepoForCmd(t)
	stubProposalSelf(t)
	host, _ := stubProposalDaemon(t)

	out, err := runCmdCaptureStdout(t, proposalStartCmd, []string{"auth-rework"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var got proposal.Summary
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
	if got.Tab != "docs" || host.tabs["auth-rework"] != "docs" {
		t.Fatalf("start must open the Docs tab and print it, got tab %q", got.Tab)
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
	var list []proposal.Summary
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
		{proposalRemoveDocCmd, []string{"p", "goals"}},
	} {
		_, err := runCmdCaptureStdout(t, tc.cmd, tc.args)
		if err == nil || !strings.Contains(err.Error(), "AF_SESSION") || strings.Contains(err.Error(), "needs a session") {
			t.Errorf("%s: error = %v, want the lookup failure", tc.cmd.Name(), err)
		}
	}
}

func jsonKeys(t *testing.T, out []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output is not a JSON object (%q): %v", out, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestProposalCommandOutputsKeepTheirShape(t *testing.T) {
	setupRepoForCmd(t)
	stubProposalSelf(t)
	host, _ := stubProposalDaemon(t)
	if _, err := runCmdCaptureStdout(t, proposalStartCmd, []string{"p"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cmd   *cobra.Command
		args  []string
		stdin string
		want  []string
	}{
		{cmd: proposalPathCmd, args: []string{"p"}, want: []string{"path", "slug"}},
		{cmd: proposalAddDocCmd, args: []string{"p", "notes"}, want: []string{"doc", "path", "slug"}},
		{cmd: proposalRemoveDocCmd, args: []string{"p", "notes"}, want: []string{"doc", "path", "slug"}},
		{cmd: proposalCatCmd, args: []string{"p", "goals"}, want: []string{"doc", "slug", "text"}},
		{cmd: proposalWriteCmd, args: []string{"p", "goals"}, stdin: "new\n", want: []string{"author", "commit", "doc", "path", "slug"}},
		{cmd: proposalCheckpointCmd, want: []string{"commit", "cursor", "slug", "unseen"}},
		{cmd: proposalChangesCmd, want: []string{"edits", "from", "slug", "to"}},
		{cmd: proposalPushModeCmd, args: []string{"manual"}, want: []string{"due_at", "due_in_ms", "mode", "session_id", "slug", "state", "unsent"}},
		{cmd: proposalSendCmd, want: []string{"due_at", "due_in_ms", "mode", "session_id", "slug", "state", "unsent"}},
		{cmd: proposalDetachCmd, want: []string{"detached", "slug"}},
	}
	for _, tc := range cases {
		tc.cmd.SetIn(strings.NewReader(tc.stdin))
		out, err := runCmdCaptureStdout(t, tc.cmd, tc.args)
		tc.cmd.SetIn(nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.cmd.Name(), err)
		}
		if got := jsonKeys(t, out); !slices.Equal(got, tc.want) {
			t.Errorf("%s output keys = %v, want %v", tc.cmd.Name(), got, tc.want)
		}
	}
	if _, ok := host.tabs["p"]; ok {
		t.Fatal("detach must close the Docs tab")
	}
}

func TestProposalWriteAuthorFollowsTheSession(t *testing.T) {
	setupRepoForCmd(t)
	stubProposalSelf(t)
	stubProposalDaemon(t)
	if _, err := runCmdCaptureStdout(t, proposalStartCmd, []string{"p"}); err != nil {
		t.Fatal(err)
	}
	proposalWriteCmd.SetIn(strings.NewReader("by agent\n"))
	defer proposalWriteCmd.SetIn(nil)
	out, err := runCmdCaptureStdout(t, proposalWriteCmd, []string{"p", "goals"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil || got["author"] != "agent" || got["commit"] == "" {
		t.Fatalf("write in a session = %s, %v; want an agent commit", out, err)
	}

	t.Setenv("TMUX", "")
	proposalWriteCmd.SetIn(strings.NewReader("by someone\n"))
	out, err = runCmdCaptureStdout(t, proposalWriteCmd, []string{"p", "goals"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil || got["author"] != "external" {
		t.Fatalf("write outside a session = %s, %v; want external", out, err)
	}
}

func TestProposalPushModeAndSend(t *testing.T) {
	setupRepoForCmd(t)
	stubProposalSelf(t)
	host, methods := stubProposalDaemon(t)
	if _, err := runCmdCaptureStdout(t, proposalStartCmd, []string{"p", "--docs", "goals"}); err != nil {
		t.Fatal(err)
	}
	out, err := runCmdCaptureStdout(t, proposalPushModeCmd, []string{"manual"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := decodeInto[proposal.PushStatus](out)
	if err != nil || st.Mode != proposal.PushManual || st.Slug != "p" || st.SessionID != proposalTestSessionID {
		t.Fatalf("push-mode output %s: %v", out, err)
	}
	*methods = nil
	if _, err := runCmdCaptureStdout(t, proposalPushModeCmd, []string{"sometimes"}); err == nil {
		t.Fatal("want an error for an unknown mode")
	}
	if len(*methods) != 0 {
		t.Fatalf("an unknown mode must fail before any route: %v", *methods)
	}
	out, err = runCmdCaptureStdout(t, proposalSendCmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st, err = decodeInto[proposal.PushStatus](out); err != nil || st.Unsent != 0 || len(host.prompts) != 0 {
		t.Fatalf("send with nothing unsent: %s, %v, %d prompts", out, err, len(host.prompts))
	}
}
