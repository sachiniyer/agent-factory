package proposal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeHost struct {
	mu       sync.Mutex
	sessions map[string]Session
	tabs     map[string]string // session id + "/" + slug → tab name
	tabErr   error
	closeErr error
	events   []Change
	prompts  []string
	delivery Delivery
	sendErr  error
	pushes   []PushStatus
	onSend   func()
}

func (h *fakeHost) SendPrompt(sessionID, prompt string) (Delivery, error) {
	if f := h.onSend; f != nil {
		h.onSend = nil
		f()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prompts = append(h.prompts, prompt)
	return h.delivery, h.sendErr
}

func (h *fakeHost) PublishPush(st PushStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pushes = append(h.pushes, st)
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		sessions: map[string]Session{
			sessA: {ID: sessA, Title: "alpha", RepoID: testRepoID},
			sessB: {ID: sessB, Title: "beta", RepoID: testRepoID},
		},
		tabs: map[string]string{},
	}
}

func (h *fakeHost) Session(id string) (Session, error) {
	s, ok := h.sessions[id]
	if !ok {
		return Session{}, errors.New("session not found")
	}
	return s, nil
}

func (h *fakeHost) SessionTitles(repoID string) map[string]string {
	out := map[string]string{}
	for id, s := range h.sessions {
		if s.RepoID == repoID {
			out[id] = s.Title
		}
	}
	return out
}

func (h *fakeHost) EnsureDocsTab(sessionID, slug string) (string, error) {
	if h.tabErr != nil {
		return "", h.tabErr
	}
	h.tabs[sessionID+"/"+slug] = "docs"
	return "docs", nil
}

func (h *fakeHost) CloseDocsTabs(sessionID, slug string) (string, error) {
	if h.closeErr != nil {
		return "", h.closeErr
	}
	name, ok := h.tabs[sessionID+"/"+slug]
	if !ok {
		return "", nil
	}
	delete(h.tabs, sessionID+"/"+slug)
	return name, nil
}

func (h *fakeHost) Publish(c Change) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, c)
}

func (h *fakeHost) takeEvents() []Change {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.events
	h.events = nil
	return out
}

func newTestService(t *testing.T) (*Service, *fakeHost, *Store) {
	t.Helper()
	store, _ := newTestStore(t)
	host := newFakeHost()
	return NewService(host), host, store
}

func byRepo() Target { return Target{RepoID: testRepoID} }

func TestServiceTargetNeedsExactlyOne(t *testing.T) {
	svc, _, _ := newTestService(t)
	for _, target := range []Target{{}, {SessionID: sessA, RepoID: testRepoID}} {
		if _, err := svc.List(target); err == nil {
			t.Errorf("List(%+v) must fail", target)
		}
	}
	if _, err := svc.List(Target{SessionID: "33333333-3333-4333-8333-333333333333"}); err == nil {
		t.Error("an unknown session must fail")
	}
	if got, err := svc.List(Target{SessionID: sessA}); err != nil || len(got) != 0 {
		t.Fatalf("List by session = %v, %v; want empty", got, err)
	}
}

func TestServiceTargetRepoPath(t *testing.T) {
	svc, _, _ := newTestService(t)
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if _, err := svc.List(Target{RepoPath: dir}); err != nil {
		t.Fatalf("List by repo_path: %v", err)
	}
	if _, err := svc.List(Target{RepoPath: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("a path that is not a repo must fail")
	}
}

func TestServiceStartAttachDetachTabs(t *testing.T) {
	svc, host, _ := newTestService(t)
	info, err := svc.Start(sessA, "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Tab != "docs" || info.Rules == "" || len(info.Docs) != len(BuiltinTemplates) {
		t.Fatalf("start = %+v, want the docs tab, rules and all templates", info)
	}
	if !slices.Equal(info.AttachedSessions, []string{"alpha"}) {
		t.Fatalf("attached_sessions = %v, want [alpha]", info.AttachedSessions)
	}
	if _, err := svc.Start(sessB, "q", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Attach(sessA, "q"); err != nil {
		t.Fatal(err)
	}
	if _, ok := host.tabs[sessA+"/p"]; ok {
		t.Fatal("attaching to another proposal must close the old Docs tab")
	}
	again, err := svc.Attach(sessA, "")
	if err != nil || again.Slug != "q" {
		t.Fatalf("attach with no slug = %+v, %v; want q", again, err)
	}
	res, err := svc.Detach(sessA)
	if err != nil || !res.Detached || res.Slug != "q" || res.Tab != "docs" {
		t.Fatalf("detach = %+v, %v", res, err)
	}
	if _, ok := host.tabs[sessA+"/q"]; ok {
		t.Fatal("detach must close the Docs tab")
	}
	if _, err := svc.Attach(sessA, ""); err == nil || !strings.Contains(err.Error(), "existing proposals: p, q") {
		t.Fatalf("attach with no slug when detached = %v, want the slug list", err)
	}
}

func TestServiceTabCloseFailureKeepsAttachment(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(sessB, "q", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	host.closeErr = errors.New("session is archived")
	if _, err := svc.Attach(sessA, "q"); err == nil {
		t.Fatal("attach must fail when the old Docs tab cannot close")
	}
	if _, err := svc.Detach(sessA); err == nil {
		t.Fatal("detach must fail when the Docs tab cannot close")
	}
	if got, _ := store.Attached(sessA); got != "p" {
		t.Fatalf("attached = %q, want p kept so a retry closes its tab", got)
	}
	host.closeErr = nil
	if _, err := svc.Attach(sessA, "q"); err != nil {
		t.Fatal(err)
	}
	if _, ok := host.tabs[sessA+"/p"]; ok {
		t.Fatal("the retry must close the old Docs tab")
	}
	host.closeErr = errors.New("session is archived")
	if _, err := svc.Detach(sessA); err == nil {
		t.Fatal("detach must fail when the Docs tab cannot close")
	}
	host.closeErr = nil
	res, err := svc.Detach(sessA)
	if err != nil || res.Slug != "q" || res.Tab != "docs" {
		t.Fatalf("detach retry = %+v, %v; want q and its tab closed", res, err)
	}
}

func TestServiceRefusesOffBoxSessions(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessB, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	host.sessions[sessA] = Session{ID: sessA, Title: "alpha", RepoID: testRepoID, OffBox: true}
	if _, err := svc.Start(sessA, "q", []string{"goals"}); err == nil || !strings.Contains(err.Error(), "off-box") {
		t.Fatalf("start = %v, want the off-box refusal", err)
	}
	if err := store.RequireSlug("q"); err == nil {
		t.Fatal("a refused start must not create the proposal")
	}
	if _, err := svc.Attach(sessA, "p"); err == nil || !strings.Contains(err.Error(), "off-box") {
		t.Fatalf("attach = %v, want the off-box refusal", err)
	}
	if got, _ := store.Attached(sessA); got != "" || len(host.tabs) != 1 {
		t.Fatalf("a refused attach must change nothing, got attached %q, tabs %v", got, host.tabs)
	}
}

func TestServiceAttachReportsTabError(t *testing.T) {
	svc, host, _ := newTestService(t)
	host.tabErr = errors.New("session is not started")
	info, err := svc.Start(sessA, "p", []string{"goals"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Tab != "" || info.TabError != "session is not started" {
		t.Fatalf("start = %+v, want tab_error and no tab", info)
	}
}

func TestServiceAuthors(t *testing.T) {
	svc, _, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Write(byRepo(), "p", "goals", "x\n", AuthorAgent); err == nil {
		t.Fatal("author agent without a session must fail")
	}
	if _, err := svc.Write(byRepo(), "p", "goals", "x\n", ""); err == nil {
		t.Fatal("an empty author must fail")
	}
	res, err := svc.Write(Target{SessionID: sessB}, "p", "goals", "b\n", AuthorAgent)
	if err != nil || res.Commit == "" || res.Author != AuthorAgent || res.Path != filepath.Join(store.SlugPath("p"), "goals.md") {
		t.Fatalf("write = %+v, %v", res, err)
	}
	add, err := svc.AddDoc(Target{SessionID: sessA}, "p", "risks", AuthorHuman)
	if err != nil || add.Doc != "risks.md" || add.Commit == "" {
		t.Fatalf("add-doc = %+v, %v", add, err)
	}
	c, err := svc.Changes(sessA)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Edits) != 2 || c.Edits[0].Session != sessB || c.Edits[1].Author != AuthorHuman {
		t.Fatalf("changes = %+v, want B's agent write then a human add-doc", c.Edits)
	}
}

func TestServiceGetDetail(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"api", "goals", "notes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Write(byRepo(), "p", "api", "> [!question] Expire tokens?\n\n>[!QUESTION] And refresh?\n> [!todo] x\n", AuthorExternal); err != nil {
		t.Fatal(err)
	}
	d, err := svc.Get(Target{SessionID: sessA}, "p")
	if err != nil {
		t.Fatal(err)
	}
	want := []DocRef{{"goals", "goals.md"}, {"api", "api.md"}, {"notes", "notes.md"}}
	if !slices.Equal(d.Docs, want) {
		t.Fatalf("docs = %v, want %v", d.Docs, want)
	}
	if !slices.Equal(d.Templates, []string{"technical", "ui", "steps", "future"}) {
		t.Fatalf("templates = %v", d.Templates)
	}
	if d.OpenQuestions != 2 || d.Head == "" || !slices.Equal(d.AttachedSessions, []string{"alpha"}) {
		t.Fatalf("detail = %+v", d)
	}
	if _, err := svc.Get(byRepo(), "nope"); err == nil || !strings.Contains(err.Error(), "existing proposals: p") {
		t.Fatalf("unknown slug = %v", err)
	}
}

func TestTemplateNamesIncludeUserTemplates(t *testing.T) {
	_, home := newTestStore(t)
	dir := filepath.Join(home, "templates", "proposal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"risks.md", "goals.md", "Bad.md", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := TemplateNames()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, append(append([]string(nil), BuiltinTemplates...), "risks")) {
		t.Fatalf("TemplateNames = %v", got)
	}
}

func readFor(t *testing.T, svc *Service, doc string) *ReadResult {
	t.Helper()
	r, err := svc.Read(Target{SessionID: sessA}, "p", doc)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSavePlain(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	r := readFor(t, svc, "goals")
	host.takeEvents()
	res, err := svc.Save(Target{SessionID: sessA}, "p", "goals", "human text\n", r.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged || res.Text != "human text\n" || res.Commit == r.Commit || res.Doc != "goals.md" {
		t.Fatalf("save = %+v", res)
	}
	msg, _ := store.gitTrim("log", "-1", "--format=%B")
	if !strings.Contains(msg, "Af-Author: human") {
		t.Fatalf("save must commit as human:\n%s", msg)
	}
	ev := host.takeEvents()
	if len(ev) != 1 || ev[0].Commit == nil || *ev[0].Commit != res.Commit || ev[0].Author != AuthorHuman ||
		ev[0].Doc != "goals.md" || !slices.Equal(ev[0].SessionIDs, []string{sessA}) {
		t.Fatalf("events = %+v", ev)
	}
	same, err := svc.Save(Target{SessionID: sessA}, "p", "goals", "human text\n", res.Commit)
	if err != nil || same.Commit != res.Commit || same.Merged {
		t.Fatalf("an unchanged save = %+v, %v; want the same commit", same, err)
	}
	if ev := host.takeEvents(); len(ev) != 0 {
		t.Fatalf("an unchanged save must publish nothing, got %+v", ev)
	}
}

func TestSaveMergesCleanly(t *testing.T) {
	svc, _, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Write(byRepo(), "p", "goals", "one\ntwo\nthree\nfour\nfive\n", AuthorExternal); err != nil {
		t.Fatal(err)
	}
	r := readFor(t, svc, "goals")
	editOnDisk(t, store, "p", "goals.md", "one\ntwo\nthree\nfour\nFIVE agent\n")
	res, err := svc.Save(Target{SessionID: sessA}, "p", "goals", "ONE human\ntwo\nthree\nfour\nfive\n", r.Commit)
	if err != nil {
		t.Fatal(err)
	}
	want := "ONE human\ntwo\nthree\nfour\nFIVE agent\n"
	if !res.Merged || res.Text != want {
		t.Fatalf("save = %+v, want merged %q", res, want)
	}
	disk, _, _ := store.Read("p", "goals")
	if disk != want {
		t.Fatalf("disk = %q, want %q", disk, want)
	}
	c := mustChanges(t, store, sessA)
	var authors []Author
	for _, e := range c.Edits {
		authors = append(authors, e.Author)
	}
	if !slices.Equal(authors, []Author{AuthorExternal, AuthorExternal, AuthorHuman}) {
		t.Fatalf("authors = %v, want the write, the agent's disk edit, then the human merge", authors)
	}
}

func TestSaveConflictCommitsNothing(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	r := readFor(t, svc, "goals")
	editOnDisk(t, store, "p", "goals.md", "agent line\n")
	host.takeEvents()
	_, err := svc.Save(Target{SessionID: sessA}, "p", "goals", "human line\n", r.Commit)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("save error = %v, want a conflict", err)
	}
	v := conflict.Versions
	if v.Ours != "human line\n" || v.Theirs != "agent line\n" || v.Base != r.Text || v.Head == "" {
		t.Fatalf("conflict = %+v", v)
	}
	if conflict.APIErrorCode() != ErrorCodeConflict || conflict.HTTPStatus() != 409 {
		t.Fatal("a conflict must map to proposal_conflict and 409")
	}
	head, _ := store.head()
	if head != v.Head {
		t.Fatalf("head moved past the conflict reply: %s vs %s", head, v.Head)
	}
	if msg, _ := store.gitTrim("log", "-1", "--format=%B"); strings.Contains(msg, "Af-Author: human") {
		t.Fatal("a conflict must not commit the save")
	}
	if ev := host.takeEvents(); len(ev) != 1 || ev[0].Author != AuthorExternal {
		t.Fatalf("a conflict must publish only the external snapshot, got %+v", ev)
	}
	res, err := svc.Save(Target{SessionID: sessA}, "p", "goals", "resolved\n", v.Head)
	if err != nil || res.Merged || res.Text != "resolved\n" {
		t.Fatalf("a save on the conflict head = %+v, %v", res, err)
	}
}

func TestSaveNeedsAKnownBase(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(byRepo(), "p", "goals", "x", ""); err == nil {
		t.Fatal("a save without base must fail")
	}
	if _, err := svc.Save(byRepo(), "p", "goals", "x", "0000000000000000000000000000000000000000"); err == nil {
		t.Fatal("a save with an unknown base must fail")
	}
}

func TestSaveRejectsABaseThatIsNotACommitID(t *testing.T) {
	svc, _, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	head, err := store.Head()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "x")
	for _, base := range []string{"--output=/tmp/x", "--output=" + out, "-q", "HEAD", "master", "HEAD~1", strings.ToUpper(head), head + " ", "abc"} {
		_, err := svc.Save(byRepo(), "p", "goals", "x", base)
		if err == nil || !strings.Contains(err.Error(), "unknown base commit") {
			t.Fatalf("base %q: err = %v, want unknown base commit", base, err)
		}
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("an option base must not reach git: stat %s = %v", out, err)
	}
	if after, _ := store.Head(); after != head {
		t.Fatalf("a rejected base must commit nothing: head %s → %s", head, after)
	}
}

func TestSaveDocAddedAfterBase(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	base := readFor(t, svc, "goals").Commit
	if _, err := svc.AddDoc(byRepo(), "p", "notes", AuthorExternal); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Save(byRepo(), "p", "notes", "first\n", base)
	if err != nil || res.Text != "first\n" {
		t.Fatalf("save of a doc that is blank on both sides = %+v, %v", res, err)
	}
}

func TestReadSnapshotsTheDoc(t *testing.T) {
	svc, _, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals", "api"}); err != nil {
		t.Fatal(err)
	}
	editOnDisk(t, store, "p", "goals.md", "edited\n")
	editOnDisk(t, store, "p", "api.md", "other\n")
	r := readFor(t, svc, "goals.md")
	if r.Text != "edited\n" || r.Doc != "goals.md" {
		t.Fatalf("read = %+v", r)
	}
	at, err := store.fileAt(r.Commit, "p/goals.md")
	if err != nil || at != "edited\n" {
		t.Fatalf("the read commit holds %q, %v; want the read text", at, err)
	}
	if status, _ := store.git(nil, "status", "--porcelain"); !strings.Contains(status, "api.md") || strings.Contains(status, "goals.md") {
		t.Fatalf("read must snapshot only its doc:\n%s", status)
	}
}

func bumpMtime(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

func TestScanPublishesDiskEdits(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create("q", []string{"goals"}, Committer{Author: AuthorExternal}); err != nil {
		t.Fatal(err)
	}
	in := WatchInput{RepoIDs: []string{testRepoID}, Tabs: map[string]map[string][]string{testRepoID: {"q": {sessB}}}}
	svc.Scan(in)
	host.takeEvents()
	svc.Scan(in)
	if ev := host.takeEvents(); len(ev) != 0 {
		t.Fatalf("an idle scan must publish nothing, got %+v", ev)
	}

	editOnDisk(t, store, "p", "goals.md", "agent edit\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	editOnDisk(t, store, "q", "notes.md", "new\n")
	svc.Scan(in)
	ev := host.takeEvents()
	if len(ev) != 2 {
		t.Fatalf("events = %+v, want the p edit and the new q doc", ev)
	}
	for _, e := range ev {
		if e.Commit != nil || e.Author != AuthorExternal || e.RepoID != testRepoID {
			t.Fatalf("event = %+v, want an uncommitted external change", e)
		}
	}
	bySlug := map[string]Change{ev[0].Slug: ev[0], ev[1].Slug: ev[1]}
	if e := bySlug["p"]; e.Doc != "goals.md" || !slices.Equal(e.SessionIDs, []string{sessA}) {
		t.Fatalf("p event = %+v", e)
	}
	if e := bySlug["q"]; e.Doc != "notes.md" || !slices.Equal(e.SessionIDs, []string{sessB}) {
		t.Fatalf("q event = %+v", e)
	}
	if status, _ := store.git(nil, "status", "--porcelain"); status == "" {
		t.Fatal("scan must not commit")
	}

	if err := os.Remove(filepath.Join(store.SlugPath("q"), "notes.md")); err != nil {
		t.Fatal(err)
	}
	svc.Scan(in)
	if ev := host.takeEvents(); len(ev) != 1 || ev[0].Doc != "notes.md" {
		t.Fatalf("a deleted doc must publish, got %+v", ev)
	}
}

func TestScanSkipsTheDaemonsOwnWrites(t *testing.T) {
	svc, host, _ := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	in := WatchInput{RepoIDs: []string{testRepoID}}
	svc.Scan(in)
	if _, err := svc.Write(byRepo(), "p", "goals", "via daemon\n", AuthorExternal); err != nil {
		t.Fatal(err)
	}
	if ev := host.takeEvents(); len(ev) != 1 || ev[0].Commit == nil {
		t.Fatalf("a daemon write must publish one committed change, got %+v", ev)
	}
	svc.Scan(in)
	if ev := host.takeEvents(); len(ev) != 0 {
		t.Fatalf("the scan after a daemon write must publish nothing, got %+v", ev)
	}
}

func TestReadPublishesTheDiskEditItSnapshots(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	in := WatchInput{RepoIDs: []string{testRepoID}}
	svc.Scan(in)
	host.takeEvents()

	editOnDisk(t, store, "p", "goals.md", "disk edit\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	res, err := svc.Read(byRepo(), "p", "goals")
	if err != nil {
		t.Fatal(err)
	}
	ev := host.takeEvents()
	if len(ev) != 1 || ev[0].Commit == nil || *ev[0].Commit != res.Commit || ev[0].Author != AuthorExternal {
		t.Fatalf("a read that snapshots a disk edit must publish it, got %+v", ev)
	}
	if _, err := svc.Read(byRepo(), "p", "goals"); err != nil {
		t.Fatal(err)
	}
	svc.Scan(in)
	if ev := host.takeEvents(); len(ev) != 0 {
		t.Fatalf("an unchanged read and the next scan must publish nothing, got %+v", ev)
	}

	editOnDisk(t, store, "p", "goals.md", "another\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	saved, err := svc.Save(byRepo(), "p", "goals", "another\n", res.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if ev := host.takeEvents(); len(ev) != 1 || ev[0].Commit == nil || *ev[0].Commit != saved.Commit || ev[0].Author != AuthorExternal {
		t.Fatalf("a no-op save that snapshots a disk edit must publish it, got %+v", ev)
	}

	editOnDisk(t, store, "p", "goals.md", "disk side\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	_, err = svc.Save(byRepo(), "p", "goals", "tab side\n", saved.Commit)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("save = %v, want a conflict", err)
	}
	if ev := host.takeEvents(); len(ev) != 1 || ev[0].Commit == nil || *ev[0].Commit != conflict.Versions.Head || ev[0].Author != AuthorExternal {
		t.Fatalf("a conflicting save that snapshots a disk edit must publish it, got %+v", ev)
	}
}

func TestScanFollowsAttachChanges(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	in := WatchInput{RepoIDs: []string{testRepoID}}
	svc.Scan(in)
	if _, err := svc.Detach(sessA); err != nil {
		t.Fatal(err)
	}
	svc.Scan(in)
	editOnDisk(t, store, "p", "goals.md", "unwatched\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	svc.Scan(in)
	if ev := host.takeEvents(); len(ev) != 0 {
		t.Fatalf("a detached proposal must not be watched, got %+v", ev)
	}
}

// The #6310 sequence: a save B times out and is still on the wire, the user undoes, the
// writer's later save lands, and then B arrives. B merges cleanly against the later head
// (the undo is invisible to a three-way merge against its base), so only the fence stops it.
func TestSaveFenceRefusesALateSaveThatWouldMergeCleanly(t *testing.T) {
	target := Target{SessionID: sessA}
	for _, tc := range []struct {
		name   string
		writer string
		seq    int64
		wantOK bool
	}{
		{"without a fence the late save merges cleanly and brings B back", "", 0, true},
		{"with a fence it is refused", "tab-1", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := newTestService(t)
			if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
				t.Fatal(err)
			}
			r := readFor(t, svc, "goals")
			// the writer's save 2 (the undo, plus a tail edit) lands first
			if _, err := svc.SaveFenced(target, "p", "goals", r.Text+"tail\n", r.Commit, "tab-1", 2); err != nil {
				t.Fatal(err)
			}
			head := readFor(t, svc, "goals")
			// then its save 1 (B, made against the original base) arrives
			_, err := svc.SaveFenced(target, "p", "goals", "B edit\n"+r.Text, r.Commit, tc.writer, tc.seq)
			after := readFor(t, svc, "goals")
			if tc.wantOK {
				if err != nil || !strings.HasPrefix(after.Text, "B edit\n") {
					t.Fatalf("late save = %v, text %q; want it merged in", err, after.Text)
				}
				return
			}
			var stale *StaleSaveError
			if !errors.As(err, &stale) || stale.Last != 2 || stale.APIErrorCode() != ErrorCodeStaleSave || stale.HTTPStatus() != 409 {
				t.Fatalf("late save = %v; want a StaleSaveError fenced by seq 2", err)
			}
			if after.Commit != head.Commit || after.Text != head.Text {
				t.Fatalf("a refused save changed the doc: %+v -> %+v", head, after)
			}
		})
	}
}

func TestSaveFenceIsPerWriterAndPerDocAndOptional(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals", "api"}); err != nil {
		t.Fatal(err)
	}
	g := readFor(t, svc, "goals")
	if _, err := svc.SaveFenced(Target{SessionID: sessA}, "p", "goals", g.Text+"1\n", g.Commit, "w1", 5); err != nil {
		t.Fatal(err)
	}
	head := readFor(t, svc, "goals")
	// the same seq again (a duplicate delivery) is refused
	if _, err := svc.SaveFenced(Target{SessionID: sessA}, "p", "goals", g.Text+"1\n", g.Commit, "w1", 5); !errors.As(err, new(*StaleSaveError)) {
		t.Fatalf("a duplicate seq = %v; want stale", err)
	}
	// another writer, another doc, no writer, and no seq are not fenced by w1's seq 5
	if _, err := svc.SaveFenced(Target{SessionID: sessA}, "p", "goals", g.Text+"1\n2\n", head.Commit, "w2", 1); err != nil {
		t.Fatalf("another writer: %v", err)
	}
	a := readFor(t, svc, "api")
	if _, err := svc.SaveFenced(Target{SessionID: sessA}, "p", "api", a.Text+"x\n", a.Commit, "w1", 1); err != nil {
		t.Fatalf("another doc: %v", err)
	}
	h2 := readFor(t, svc, "goals")
	if _, err := svc.SaveFenced(Target{SessionID: sessA}, "p", "goals", h2.Text+"3\n", h2.Commit, "", 0); err != nil {
		t.Fatalf("unfenced save: %v", err)
	}
	h3 := readFor(t, svc, "goals")
	if _, err := svc.SaveFenced(Target{SessionID: sessA}, "p", "goals", h3.Text+"4\n", h3.Commit, "w1", 6); err != nil {
		t.Fatalf("a higher seq: %v", err)
	}
}

func TestFenceTableForgetsTheOldestWriter(t *testing.T) {
	var f fenceTable
	for i := 0; i < fenceCap+10; i++ {
		if ok, _ := f.admit(fmt.Sprintf("k%d", i), 1); !ok {
			t.Fatalf("first save of k%d refused", i)
		}
	}
	if len(f.seen) != fenceCap {
		t.Fatalf("remembers %d writers, want %d", len(f.seen), fenceCap)
	}
	if ok, _ := f.admit("k0", 1); !ok {
		t.Fatal("a forgotten writer is unfenced")
	}
	if ok, _ := f.admit(fmt.Sprintf("k%d", fenceCap+9), 1); ok {
		t.Fatal("a remembered writer's repeat seq must be refused")
	}
}
