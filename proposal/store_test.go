package proposal

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	testRepoID = "0123456789ab"
	sessA      = "11111111-1111-4111-8111-111111111111"
	sessB      = "22222222-2222-4222-8222-222222222222"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	s, err := Open(testRepoID)
	if err != nil {
		t.Fatal(err)
	}
	return s, home
}

func agent(id string) Committer { return Committer{Author: AuthorAgent, SessionID: id} }

func startAttached(t *testing.T, s *Store, slug, id string, docs ...string) {
	t.Helper()
	if len(docs) == 0 {
		docs = BuiltinTemplates
	}
	if err := s.Create(slug, docs, agent(id)); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(slug, id); err != nil {
		t.Fatal(err)
	}
}

func editOnDisk(t *testing.T, s *Store, slug, file, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.SlugPath(slug), file), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustChanges(t *testing.T, s *Store, id string) *Changes {
	t.Helper()
	c, err := s.Changes(id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStoreLivesUnderAFHome(t *testing.T) {
	s, home := newTestStore(t)
	want := filepath.Join(home, "repos", testRepoID, "proposals")
	if s.Dir() != want {
		t.Fatalf("Dir() = %q, want %q", s.Dir(), want)
	}
}

func TestCreateFromBuiltinTemplates(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Create("auth-rework", BuiltinTemplates, agent(sessA)); err != nil {
		t.Fatal(err)
	}
	docs, err := s.Docs("auth-rework")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, n := range BuiltinTemplates {
		want = append(want, n+".md")
	}
	if !slices.Equal(docs, want) {
		t.Fatalf("docs = %v, want %v", docs, want)
	}
	if !slices.Equal(BuiltinTemplates, []string{"goals", "technical", "api", "ui", "steps", "future"}) {
		t.Fatalf("BuiltinTemplates = %v, want goals, technical, api, ui, steps, future", BuiltinTemplates)
	}
	future, _, err := s.Read("auth-rework", "future")
	if err != nil || !strings.HasPrefix(future, "# auth rework: future") {
		t.Fatalf("future.md = %q, %v; want the rendered future template", future, err)
	}
	text, _, err := s.Read("auth-rework", "goals")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "# auth rework: goals") {
		t.Fatalf("goals.md does not start with the rendered title:\n%s", text)
	}
	if strings.Contains(text, "{{title}}") {
		t.Fatal("{{title}} was not substituted")
	}
	if status, _ := s.git(nil, "status", "--porcelain"); status != "" {
		t.Fatalf("create left uncommitted files:\n%s", status)
	}
	if err := s.Create("auth-rework", []string{"goals"}, agent(sessA)); err == nil {
		t.Fatal("creating an existing slug must fail")
	}
}

func TestUserTemplateOverride(t *testing.T) {
	s, home := newTestStore(t)
	dir := filepath.Join(home, "templates", "proposal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "goals.md"), []byte("Custom {{title}} {{other}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "risks.md"), []byte("Risks of {{title}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("search-v2", []string{"goals", "risks", "notes"}, agent(sessA)); err != nil {
		t.Fatal(err)
	}
	for doc, want := range map[string]string{
		"goals": "Custom search v2 {{other}}\n",
		"risks": "Risks of search v2\n",
		"notes": "",
	} {
		got, _, err := s.Read("search-v2", doc)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s.md = %q, want %q", doc, got, want)
		}
	}
}

func TestInvalidNames(t *testing.T) {
	s, _ := newTestStore(t)
	for _, slug := range []string{"", "Auth", "-auth", "auth_rework", "../x", "a/b", strings.Repeat("a", 64)} {
		if err := s.Create(slug, []string{"goals"}, agent(sessA)); err == nil {
			t.Errorf("Create(%q) must fail", slug)
		}
	}
	if err := s.Create("ok", []string{"Bad.Name"}, agent(sessA)); err == nil {
		t.Error("an invalid doc name must fail")
	}
	if err := s.Create("ok", []string{"goals"}, agent("../../x")); err == nil {
		t.Error("an invalid session id must fail")
	}
	if err := ValidateSlug(strings.Repeat("a", 63)); err != nil {
		t.Errorf("a 63-character slug is valid: %v", err)
	}
}

func TestUnknownSlugListsExisting(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "alpha", sessA, "goals")
	err := s.Attach("beta", sessA)
	if err == nil || !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("error must list the existing slugs, got %v", err)
	}
}

func TestChangesExcludesOwnCheckpoint(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if c := mustChanges(t, s, sessA); len(c.Edits) != 0 {
		t.Fatalf("a fresh proposal must have no changes, got %+v", c.Edits)
	}
	editOnDisk(t, s, "p", "goals.md", "agent edit\n")
	cp, err := s.Checkpoint(sessA)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Commit == "" || cp.Unseen != 0 {
		t.Fatalf("checkpoint = %+v, want a commit and no unseen edits", cp)
	}
	if c := mustChanges(t, s, sessA); len(c.Edits) != 0 {
		t.Fatalf("own checkpoint must not show in changes, got %+v", c.Edits)
	}
}

func TestChangesIncludesExternalAndOtherSessions(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := s.Attach("p", sessB); err != nil {
		t.Fatal(err)
	}

	editOnDisk(t, s, "p", "api.md", "B edit\n")
	if _, err := s.Checkpoint(sessB); err != nil {
		t.Fatal(err)
	}
	editOnDisk(t, s, "p", "goals.md", "user edit\n")

	c := mustChanges(t, s, sessA)
	if len(c.Edits) != 2 {
		t.Fatalf("want 2 edits, got %+v", c.Edits)
	}
	if e := c.Edits[0]; e.Author != AuthorAgent || e.Session != sessB || !slices.Equal(e.Files, []string{"api.md"}) {
		t.Errorf("first edit = %+v, want session B's agent edit of api.md", e)
	}
	if e := c.Edits[1]; e.Author != AuthorExternal || e.Session != "" || !slices.Equal(e.Files, []string{"goals.md"}) {
		t.Errorf("second edit = %+v, want an external edit of goals.md", e)
	}
	if !strings.Contains(c.Edits[1].Patch, "+user edit") {
		t.Errorf("patch must carry the edit:\n%s", c.Edits[1].Patch)
	}
	if c.Edits[1].At == "" {
		t.Error("edit must carry a time")
	}
}

func TestChangesMovesCursor(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	editOnDisk(t, s, "p", "goals.md", "user edit\n")
	first := mustChanges(t, s, sessA)
	if len(first.Edits) != 1 {
		t.Fatalf("want 1 edit, got %+v", first.Edits)
	}
	head, _ := s.head()
	if first.To != head {
		t.Fatalf("to = %s, want HEAD %s", first.To, head)
	}
	second := mustChanges(t, s, sessA)
	if len(second.Edits) != 0 || second.From != first.To {
		t.Fatalf("second changes = %+v, want no edits from %s", second, first.To)
	}
}

func TestChangesIgnoresOtherProposals(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := s.Create("q", []string{"goals"}, agent(sessB)); err != nil {
		t.Fatal(err)
	}
	editOnDisk(t, s, "q", "goals.md", "other proposal\n")
	if c := mustChanges(t, s, sessA); len(c.Edits) != 0 {
		t.Fatalf("edits in another proposal must not show, got %+v", c.Edits)
	}
}

func TestCheckpointKeepsUnseenEdits(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := s.Attach("p", sessB); err != nil {
		t.Fatal(err)
	}
	editOnDisk(t, s, "p", "api.md", "B edit\n")
	if _, err := s.Checkpoint(sessB); err != nil {
		t.Fatal(err)
	}
	editOnDisk(t, s, "p", "goals.md", "A edit\n")
	cp, err := s.Checkpoint(sessA)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Unseen != 1 {
		t.Fatalf("checkpoint = %+v, want 1 unseen edit", cp)
	}
	c := mustChanges(t, s, sessA)
	if len(c.Edits) != 1 || c.Edits[0].Session != sessB {
		t.Fatalf("changes after checkpoint = %+v, want only session B's edit", c.Edits)
	}
}

func TestCheckpointSnapshotsOtherProposalsAsExternal(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := s.Create("q", []string{"goals"}, agent(sessB)); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach("q", sessB); err != nil {
		t.Fatal(err)
	}
	editOnDisk(t, s, "q", "goals.md", "edit in q\n")
	if _, err := s.Checkpoint(sessA); err != nil {
		t.Fatal(err)
	}
	c := mustChanges(t, s, sessB)
	if len(c.Edits) != 1 || c.Edits[0].Author != AuthorExternal {
		t.Fatalf("an edit outside A's proposal must be external, got %+v", c.Edits)
	}
}

func TestReattachSwitchesProposal(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := s.Create("q", []string{"goals"}, agent(sessA)); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach("q", sessA); err != nil {
		t.Fatal(err)
	}
	got, err := s.Attached(sessA)
	if err != nil || got != "q" {
		t.Fatalf("Attached = %q, %v; want q", got, err)
	}
	all, err := s.Attachments()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[sessA] != "q" {
		t.Fatalf("Attachments = %v, want only %s → q", all, sessA)
	}
	editOnDisk(t, s, "p", "goals.md", "edit in p\n")
	if c := mustChanges(t, s, sessA); c.Slug != "q" || len(c.Edits) != 0 {
		t.Fatalf("changes = %+v, want q with no edits", c)
	}
}

func TestReattachSameSlugKeepsCursor(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if _, _, err := s.Write("p", "goals", "user edit\n", Committer{Author: AuthorExternal}); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach("p", sessA); err != nil {
		t.Fatal(err)
	}
	if c := mustChanges(t, s, sessA); len(c.Edits) != 1 {
		t.Fatalf("re-attach must not skip unread edits, got %+v", c.Edits)
	}
}

func TestDetach(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	slug, err := s.Detach(sessA)
	if err != nil || slug != "p" {
		t.Fatalf("Detach = %q, %v; want p", slug, err)
	}
	if got, _ := s.Attached(sessA); got != "" {
		t.Fatalf("still attached to %q", got)
	}
	if out, _ := s.gitTrim("for-each-ref", "refs/af/"); out != "" {
		t.Fatalf("detach left refs:\n%s", out)
	}
	if _, err := s.Changes(sessA); err != ErrNotAttached {
		t.Fatalf("Changes after detach = %v, want ErrNotAttached", err)
	}
	if _, err := s.Checkpoint(sessA); err != ErrNotAttached {
		t.Fatalf("Checkpoint after detach = %v, want ErrNotAttached", err)
	}
}

func TestRemoveDoc(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA, "goals", "api")
	if err := os.WriteFile(filepath.Join(s.SlugPath("p"), "goals.md"), []byte("unsaved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, commit, err := s.RemoveDoc("p", "api", Committer{Author: AuthorHuman})
	if err != nil || file != "api.md" || commit == "" {
		t.Fatalf("RemoveDoc = %q, %q, %v", file, commit, err)
	}
	if docs, _ := s.Docs("p"); !slices.Equal(docs, []string{"goals.md"}) {
		t.Fatalf("docs after remove = %v", docs)
	}
	if _, _, err := s.RemoveDoc("p", "api", Committer{Author: AuthorHuman}); err == nil {
		t.Fatal("removing a missing doc must fail")
	}
	if text, err := s.git(nil, "show", commit+"^:p/api.md"); err != nil || text == "" {
		t.Fatalf("the removed doc must stay in history: %q, %v", text, err)
	}
	if text, err := s.git(nil, "show", commit+":p/goals.md"); err != nil || text != "unsaved\n" {
		t.Fatalf("uncommitted edits to other docs must be snapshotted first: %q, %v", text, err)
	}
	c := mustChanges(t, s, sessA)
	if len(c.Edits) != 2 {
		t.Fatalf("want the snapshot and the removal as 2 edits, got %+v", c.Edits)
	}
}

func TestWriteAndAddDoc(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA, "goals")
	file, _, err := s.AddDoc("p", "risks", Committer{Author: AuthorExternal})
	if err != nil || file != "risks.md" {
		t.Fatalf("AddDoc = %q, %v", file, err)
	}
	if _, _, err := s.AddDoc("p", "risks", Committer{Author: AuthorExternal}); err == nil {
		t.Fatal("adding an existing doc must fail")
	}
	if _, commit, err := s.Write("p", "risks.md", "new text\n", Committer{Author: AuthorExternal}); err != nil || commit == "" {
		t.Fatalf("Write = %q, %v", commit, err)
	}
	if _, _, err := s.Write("p", "missing", "x", Committer{Author: AuthorExternal}); err == nil {
		t.Fatal("writing a missing doc must fail")
	}
	c := mustChanges(t, s, sessA)
	if len(c.Edits) != 2 {
		t.Fatalf("want add-doc and write as 2 edits, got %+v", c.Edits)
	}
	if _, _, err := s.Write("p", "goals", "own\n", agent(sessA)); err != nil {
		t.Fatal(err)
	}
	if c := mustChanges(t, s, sessA); len(c.Edits) != 0 {
		t.Fatalf("own agent write must not show, got %+v", c.Edits)
	}
}

func TestCommitsIgnoreUserGitConfig(t *testing.T) {
	s, home := newTestStore(t)
	gitconfig := filepath.Join(home, "gitconfig")
	hooks := filepath.Join(home, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[user]\n\tname = Someone\n[core]\n\thooksPath = " + hooks + "\n[commit]\n\tgpgSign = true\n"
	if err := os.WriteFile(gitconfig, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_DIR", filepath.Join(home, "elsewhere"))
	startAttached(t, s, "p", sessA, "goals")
	who, err := s.gitTrim("log", "-1", "--format=%an <%ae> / %cn <%ce>")
	if err != nil {
		t.Fatal(err)
	}
	want := committerName + " <" + committerEmail + "> / " + committerName + " <" + committerEmail + ">"
	if who != want {
		t.Fatalf("identity = %q, want %q", who, want)
	}
	msg, _ := s.gitTrim("log", "-1", "--format=%B")
	if !strings.Contains(msg, "Af-Author: agent") || !strings.Contains(msg, "Af-Session: "+sessA) {
		t.Fatalf("commit message lacks trailers:\n%s", msg)
	}
}

func TestListHelpers(t *testing.T) {
	s, _ := newTestStore(t)
	if slugs, err := s.Slugs(); err != nil || len(slugs) != 0 {
		t.Fatalf("an unused store lists %v, %v", slugs, err)
	}
	if _, err := os.Stat(s.Dir()); !os.IsNotExist(err) {
		t.Fatal("listing must not create the store")
	}
	startAttached(t, s, "b", sessA, "notes", "future", "ui", "goals")
	docs, _ := s.Docs("b")
	if !slices.Equal(docs, []string{"goals.md", "ui.md", "future.md", "notes.md"}) {
		t.Fatalf("docs = %v, want template order then alphabetical", docs)
	}
	if at, err := s.LastEdit("b"); err != nil || at.IsZero() {
		t.Fatalf("LastEdit = %v, %v", at, err)
	}
}

func TestReattachSameSlugRepairsMissingCursor(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if _, err := s.git(nil, "update-ref", "-d", cursorRefPrefix+sessA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Changes(sessA); err == nil {
		t.Fatal("changes without a cursor must fail")
	}
	if err := s.Attach("p", sessA); err != nil {
		t.Fatal(err)
	}
	if c := mustChanges(t, s, sessA); c.Slug != "p" {
		t.Fatalf("changes after repair = %+v", c)
	}
}

func TestCreateRemovesFolderOnFailure(t *testing.T) {
	s, home := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(home, "templates", "proposal", "goals.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("broken", []string{"technical", "goals"}, agent(sessA)); err == nil {
		t.Fatal("an unreadable template must fail Create")
	}
	if _, err := os.Stat(s.SlugPath("broken")); !os.IsNotExist(err) {
		t.Fatalf("the proposal folder must be removed, stat err = %v", err)
	}
	if status, _ := s.git(nil, "status", "--porcelain"); status != "" {
		t.Fatalf("a failed create left changes:\n%s", status)
	}
}

func TestRulesExplainPushedEdits(t *testing.T) {
	r := Rules("p", "/x/p")
	if !strings.Contains(r, `arrive as a prompt that starts with "Proposal p:"`) || !strings.Contains(r, `you do not need to run "af proposal changes" first`) {
		t.Fatalf("rule 2 must explain pushed edits:\n%s", r)
	}
}
