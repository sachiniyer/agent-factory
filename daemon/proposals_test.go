package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/proposal"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

func postProposalRoute(t *testing.T, cs *controlServer, verb string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	rec := doHTTP(cs, http.MethodPost, "/v1/"+verb, string(raw))
	var env map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
	return rec.Code, env
}

func docsTabsOf(t *testing.T, m *Manager, id string) []*session.Tab {
	t.Helper()
	instance, _, _, _, _, err := m.resolveActionSession(id, "", "")
	require.NoError(t, err)
	var out []*session.Tab
	for _, tab := range instance.GetTabs() {
		if tab.Kind == session.TabKindDocs {
			out = append(out, tab)
		}
	}
	return out
}

func TestProposalRoutes_StartOpensDocsTabAndDetachClosesIt(t *testing.T) {
	manager, _, data := createRealKillSession(t, "planner")
	cs := &controlServer{manager: manager}

	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "auth", Docs: []string{"goals"}})
	require.Equal(t, http.StatusOK, code, env)
	started := env["data"].(map[string]any)
	require.Equal(t, "docs", started["tab"])
	require.Contains(t, started["rules"], "af proposal checkpoint")

	tabs := docsTabsOf(t, manager, data.ID)
	require.Len(t, tabs, 1)
	require.Equal(t, "auth", tabs[0].Proposal)

	code, env = postProposalRoute(t, cs, "AttachProposal", AttachProposalRequest{SessionID: data.ID})
	require.Equal(t, http.StatusOK, code, env)
	require.Equal(t, "docs", env["data"].(map[string]any)["tab"])
	require.Len(t, docsTabsOf(t, manager, data.ID), 1, "attach must reuse the Docs tab")

	code, env = postProposalRoute(t, cs, "DetachProposal", DetachProposalRequest{SessionID: data.ID})
	require.Equal(t, http.StatusOK, code, env)
	require.Equal(t, map[string]any{"slug": "auth", "detached": true, "tab": "docs"}, env["data"])
	require.Empty(t, docsTabsOf(t, manager, data.ID))
}

func TestProposalRoutes_StartRefusesOffBoxSession(t *testing.T) {
	manager, _, data := createRealKillSession(t, "sandboxed")
	instance, _, _, _, _, err := manager.resolveActionSession(data.ID, "", "")
	require.NoError(t, err)
	instance.SetBackend(remoteTypeBackend{session.NewFakeBackend()})
	cs := &controlServer{manager: manager}

	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "auth", Docs: []string{"goals"}})
	require.NotEqual(t, http.StatusOK, code, env)
	require.Contains(t, env["error"].(map[string]any)["message"], "off-box")
	require.Empty(t, docsTabsOf(t, manager, data.ID))
}

func TestProposalRoutes_SaveMergesAndConflictsWith409(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "editor")
	cs := &controlServer{manager: manager}
	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "p", Docs: []string{"goals"}})
	require.Equal(t, http.StatusOK, code, env)

	code, env = postProposalRoute(t, cs, "ReadProposalDoc", ReadProposalDocRequest{SessionID: data.ID, Slug: "p", Doc: "goals"})
	require.Equal(t, http.StatusOK, code, env)
	read := env["data"].(map[string]any)
	base := read["commit"].(string)
	require.NotEmpty(t, base)

	code, env = postProposalRoute(t, cs, "SaveProposalDoc", SaveProposalDocRequest{SessionID: data.ID, Slug: "p", Doc: "goals", Text: "human\n", Base: base})
	require.Equal(t, http.StatusOK, code, env)
	saved := env["data"].(map[string]any)
	require.Equal(t, false, saved["merged"])
	require.Equal(t, "goals.md", saved["doc"])

	store, err := proposal.Open(repo.ID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(store.SlugPath("p"), "goals.md"), []byte("agent\n"), 0o644))

	code, env = postProposalRoute(t, cs, "SaveProposalDoc", SaveProposalDocRequest{RepoID: repo.ID, Slug: "p", Doc: "goals.md", Text: "human again\n", Base: saved["commit"].(string)})
	require.Equal(t, http.StatusConflict, code, env)
	errBody := env["error"].(map[string]any)
	require.Equal(t, proposal.ErrorCodeConflict, errBody["code"])
	versions := env["data"].(map[string]any)
	require.Equal(t, "human again\n", versions["ours"])
	require.Equal(t, "agent\n", versions["theirs"])
	require.Equal(t, "human\n", versions["base"])
	require.NotEmpty(t, versions["head"])
}

func TestProposalRoutes_TargetAndAuthorErrors(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "targets")
	cs := &controlServer{manager: manager}
	code, env := postProposalRoute(t, cs, "ListProposals", ListProposalsRequest{SessionID: data.ID, RepoID: repo.ID})
	require.Equal(t, http.StatusInternalServerError, code)
	require.Contains(t, env["error"].(map[string]any)["message"], "exactly one of session_id, repo_id or repo_path")

	code, env = postProposalRoute(t, cs, "ListProposals", ListProposalsRequest{RepoPath: repo.Root})
	require.Equal(t, http.StatusOK, code, env)
	require.Equal(t, []any{}, env["data"].(map[string]any)["proposals"])

	code, env = postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "p", Docs: []string{"goals"}})
	require.Equal(t, http.StatusOK, code, env)
	code, env = postProposalRoute(t, cs, "WriteProposalDoc", WriteProposalDocRequest{RepoID: repo.ID, Slug: "p", Doc: "goals", Text: "x", Author: "agent"})
	require.Equal(t, http.StatusInternalServerError, code)
	require.Contains(t, env["error"].(map[string]any)["message"], "author agent needs session_id")
}

func TestCreateTab_DocsNeedsAnExistingProposal(t *testing.T) {
	manager, _, data := createRealKillSession(t, "docstab")
	_, err := manager.CreateTab(CreateTabRequest{ID: data.ID, Kind: "docs", Proposal: "missing"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
	_, err = manager.CreateTab(CreateTabRequest{ID: data.ID, Kind: "docs"})
	require.ErrorContains(t, err, "requires a proposal")
	_, err = manager.CreateTab(CreateTabRequest{ID: data.ID, Kind: "web", URL: "https://example.com", Proposal: "p"})
	require.ErrorContains(t, err, "only valid for a docs tab")
}

func nextProposalEvent(t *testing.T, ch <-chan agentproto.Event) proposal.Change {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != agentproto.EventProposalChanged {
				continue
			}
			var c proposal.Change
			require.NoError(t, json.Unmarshal(ev.Data, &c))
			return c
		case <-deadline:
			t.Fatal("no proposal.changed event")
		}
	}
}

func TestProposalEvents_SaveAndDiskEdit(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "watcher")
	cs := &controlServer{manager: manager}
	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "p", Docs: []string{"goals"}})
	require.Equal(t, http.StatusOK, code, env)
	manager.ScanProposals()

	id, ch, _ := manager.events.subscribeWithOverflow()
	defer manager.events.unsubscribe(id)

	code, env = postProposalRoute(t, cs, "WriteProposalDoc", WriteProposalDocRequest{SessionID: data.ID, Slug: "p", Doc: "goals", Text: "agent write\n", Author: "agent"})
	require.Equal(t, http.StatusOK, code, env)
	c := nextProposalEvent(t, ch)
	require.Equal(t, repo.ID, c.RepoID)
	require.Equal(t, "goals.md", c.Doc)
	require.Equal(t, proposal.AuthorAgent, c.Author)
	require.NotNil(t, c.Commit)
	require.Equal(t, []string{data.ID}, c.SessionIDs)

	store, err := proposal.Open(repo.ID)
	require.NoError(t, err)
	path := filepath.Join(store.SlugPath("p"), "goals.md")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("disk edit\n", 3)), 0o644))
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, future, future))
	manager.ScanProposals()
	c = nextProposalEvent(t, ch)
	require.Nil(t, c.Commit)
	require.Equal(t, proposal.AuthorExternal, c.Author)
	require.Equal(t, "goals.md", c.Doc)
}

func TestProposalRoutes_SaveWithAStaleSeqIsRefusedWith409(t *testing.T) {
	manager, _, data := createRealKillSession(t, "fenced")
	cs := &controlServer{manager: manager}
	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "p", Docs: []string{"goals"}})
	require.Equal(t, http.StatusOK, code, env)
	code, env = postProposalRoute(t, cs, "ReadProposalDoc", ReadProposalDocRequest{SessionID: data.ID, Slug: "p", Doc: "goals"})
	require.Equal(t, http.StatusOK, code, env)
	base := env["data"].(map[string]any)["commit"].(string)

	code, env = postProposalRoute(t, cs, "SaveProposalDoc", SaveProposalDocRequest{SessionID: data.ID, Slug: "p", Doc: "goals", Text: "second\n", Base: base, Writer: "tab", Seq: 2})
	require.Equal(t, http.StatusOK, code, env)
	code, env = postProposalRoute(t, cs, "SaveProposalDoc", SaveProposalDocRequest{SessionID: data.ID, Slug: "p", Doc: "goals", Text: "late\n", Base: base, Writer: "tab", Seq: 1})
	require.Equal(t, http.StatusConflict, code, env)
	require.Equal(t, proposal.ErrorCodeStaleSave, env["error"].(map[string]any)["code"])

	code, env = postProposalRoute(t, cs, "ReadProposalDoc", ReadProposalDocRequest{SessionID: data.ID, Slug: "p", Doc: "goals"})
	require.Equal(t, http.StatusOK, code, env)
	require.Equal(t, "second\n", env["data"].(map[string]any)["text"], "the refused save changed nothing")
}
