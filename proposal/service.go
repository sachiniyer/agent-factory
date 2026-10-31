package proposal

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// Session is what the service needs to know about an af session.
type Session struct {
	ID     string
	Title  string
	RepoID string
	// OffBox is true when the session's workspace is not on the daemon host
	// (docker, ssh, sandbox), so its agent cannot open the store's files.
	OffBox bool
}

// Host is the daemon side of the service: session lookup, the session's Docs
// tabs, and the events stream.
type Host interface {
	// Session resolves a session by its stable id.
	Session(id string) (Session, error)
	// SessionTitles maps the session ids of project repoID to titles.
	SessionTitles(repoID string) map[string]string
	// EnsureDocsTab returns the name of the session's Docs tab for slug, and
	// creates the tab when there is none.
	EnsureDocsTab(sessionID, slug string) (string, error)
	// CloseDocsTabs closes the session's Docs tabs for slug and returns the
	// name of one closed tab, or "".
	CloseDocsTabs(sessionID, slug string) (string, error)
	// Publish sends a proposal.changed event.
	Publish(Change)
	// SendPrompt delivers a push prompt to the session through the same path
	// as af sessions send-prompt.
	SendPrompt(sessionID, prompt string) (Delivery, error)
	// PublishPush sends a proposal.push event.
	PublishPush(PushStatus)
}

// Change is the payload of a proposal.changed event. Commit is nil for an
// edit seen on disk that is not committed yet.
type Change struct {
	RepoID     string   `json:"repo_id"`
	SessionIDs []string `json:"session_ids"`
	Slug       string   `json:"slug"`
	Doc        string   `json:"doc"`
	Commit     *string  `json:"commit"`
	Author     Author   `json:"author"`
}

// Target names the project of a request: exactly one of a session id, a repo
// id, or a path inside the repo on the daemon host.
type Target struct {
	SessionID string
	RepoID    string
	RepoPath  string
}

// Summary describes one proposal. Start and attach fill Rules and the tab
// fields.
type Summary struct {
	Slug             string   `json:"slug"`
	Path             string   `json:"path"`
	Docs             []string `json:"docs"`
	AttachedSessions []string `json:"attached_sessions"`
	LastEdit         string   `json:"last_edit,omitempty"`
	Rules            string   `json:"rules,omitempty"`
	Tab              string   `json:"tab,omitempty"`
	TabError         string   `json:"tab_error,omitempty"`
}

// DetachResult is the result of Service.Detach.
type DetachResult struct {
	Slug     string `json:"slug"`
	Detached bool   `json:"detached"`
	Tab      string `json:"tab"`
}

// DocResult is the result of a write to one doc.
type DocResult struct {
	Slug   string `json:"slug"`
	Doc    string `json:"doc"`
	Path   string `json:"path"`
	Commit string `json:"commit"`
	Author Author `json:"author,omitempty"`
}

// ReadResult is the result of Service.Read.
type ReadResult struct {
	Slug   string `json:"slug"`
	Doc    string `json:"doc"`
	Text   string `json:"text"`
	Commit string `json:"commit"`
}

// SaveDocResult is the result of Service.Save.
type SaveDocResult struct {
	Slug string `json:"slug"`
	Doc  string `json:"doc"`
	SaveResult
}

// Service runs proposal operations for the daemon.
type Service struct {
	host Host

	mu      sync.Mutex
	watch   watchState
	lastSet map[string]map[string][]string
	// push maps session id → push state.
	push  map[string]*pushEntry
	now   func() time.Time
	spawn func(func())
	// fence refuses a save that arrives after a later save of the same writer.
	fence fenceTable
}

// NewService returns a service that uses host.
func NewService(host Host) *Service {
	return &Service{
		host: host, watch: newWatchState(), push: map[string]*pushEntry{},
		now: time.Now, spawn: func(f func()) { go f() },
	}
}

type resolved struct {
	store   *Store
	repoID  string
	session *Session
}

func (s *Service) resolve(t Target) (*resolved, error) {
	set := 0
	for _, v := range []string{t.SessionID, t.RepoID, t.RepoPath} {
		if strings.TrimSpace(v) != "" {
			set++
		}
	}
	if set != 1 {
		return nil, errors.New("name the project with exactly one of session_id, repo_id or repo_path")
	}
	r := &resolved{}
	switch {
	case t.SessionID != "":
		sess, err := s.session(t.SessionID)
		if err != nil {
			return nil, err
		}
		r.session, r.repoID = sess, sess.RepoID
	case t.RepoID != "":
		r.repoID = t.RepoID
	default:
		path, err := config.ResolveDaemonHostPath(t.RepoPath)
		if err != nil {
			return nil, err
		}
		repo, err := config.RepoFromPath(path)
		if err != nil {
			return nil, fmt.Errorf("repo_path %q: %w", t.RepoPath, err)
		}
		r.repoID = repo.ID
	}
	store, err := Open(r.repoID)
	if err != nil {
		return nil, err
	}
	r.store = store
	return r, nil
}

func (s *Service) session(id string) (*Session, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("session_id is required")
	}
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	sess, err := s.host.Session(id)
	if err != nil {
		return nil, err
	}
	if sess.RepoID == "" {
		return nil, fmt.Errorf("cannot resolve the project of session %q", sess.Title)
	}
	return &sess, nil
}

func (s *Service) sessionTarget(id string) (*resolved, error) {
	return s.resolve(Target{SessionID: id})
}

func (s *Service) summary(r *resolved, slug string) (*Summary, error) {
	docs, err := r.store.Docs(slug)
	if err != nil {
		return nil, err
	}
	ids, err := s.attachedTo(r, slug)
	if err != nil {
		return nil, err
	}
	return &Summary{
		Slug:             slug,
		Path:             r.store.SlugPath(slug),
		Docs:             docs,
		AttachedSessions: s.titles(r.repoID, ids),
	}, nil
}

func (s *Service) attachedTo(r *resolved, slug string) ([]string, error) {
	attachments, err := r.store.Attachments()
	if err != nil {
		return nil, err
	}
	var ids []string
	for id, sl := range attachments {
		if sl == slug {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// titles maps session ids to titles. An unknown id maps to itself, so a stale
// attachment still shows.
func (s *Service) titles(repoID string, ids []string) []string {
	byID := s.host.SessionTitles(repoID)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if t, ok := byID[id]; ok {
			out = append(out, t)
		} else {
			out = append(out, id)
		}
	}
	return out
}

// List describes every proposal of the project, newest edit first.
func (s *Service) List(t Target) ([]Summary, error) {
	r, err := s.resolve(t)
	if err != nil {
		return nil, err
	}
	slugs, err := r.store.Slugs()
	if err != nil {
		return nil, err
	}
	out := []Summary{}
	edits := map[string]time.Time{}
	for _, slug := range slugs {
		info, err := s.summary(r, slug)
		if err != nil {
			return nil, err
		}
		at, err := r.store.LastEdit(slug)
		if err != nil {
			return nil, err
		}
		edits[slug] = at
		info.LastEdit = at.Format(time.RFC3339)
		out = append(out, *info)
	}
	sort.SliceStable(out, func(i, j int) bool { return edits[out[i].Slug].After(edits[out[j].Slug]) })
	return out, nil
}

// Get describes proposal slug for the Docs tab.
func (s *Service) Get(t Target, slug string) (*Detail, error) {
	r, err := s.resolve(t)
	if err != nil {
		return nil, err
	}
	d, err := r.store.Detail(slug)
	if err != nil {
		return nil, err
	}
	ids, err := s.attachedTo(r, slug)
	if err != nil {
		return nil, err
	}
	d.AttachedSessions = s.titles(r.repoID, ids)
	return d, nil
}

// Start creates proposal slug for the session, attaches the session, and opens
// its Docs tab.
func (s *Service) Start(sessionID, slug string, docs []string) (*Summary, error) {
	if err := ValidateSlug(slug); err != nil {
		return nil, err
	}
	r, err := s.sessionTarget(sessionID)
	if err != nil {
		return nil, err
	}
	if err := refuseOffBox(r); err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		docs = BuiltinTemplates
	}
	before, err := r.store.Head()
	if err != nil {
		return nil, err
	}
	if err := r.store.Create(slug, docs, agentCommitter(r)); err != nil {
		return nil, err
	}
	if own, err := r.store.Head(); err == nil {
		s.publishSnapshot(r, before, own)
	}
	return s.attach(r, slug)
}

// Attach attaches the session to proposal slug and opens its Docs tab. With
// an empty slug it reports the current proposal again.
func (s *Service) Attach(sessionID, slug string) (*Summary, error) {
	r, err := s.sessionTarget(sessionID)
	if err != nil {
		return nil, err
	}
	if slug == "" {
		current, err := r.store.Attached(r.session.ID)
		if err != nil {
			return nil, err
		}
		if current == "" {
			return nil, s.notAttached(r)
		}
		slug = current
	}
	return s.attach(r, slug)
}

func refuseOffBox(r *resolved) error {
	if !r.session.OffBox {
		return nil
	}
	return fmt.Errorf("session %q runs off-box (docker/ssh/sandbox), so its agent cannot open the proposal docs on the daemon host: start or attach the proposal from a session with a local worktree", r.session.Title)
}

func (s *Service) attach(r *resolved, slug string) (*Summary, error) {
	if err := refuseOffBox(r); err != nil {
		return nil, err
	}
	previous, err := r.store.Attached(r.session.ID)
	if errors.Is(err, errBadAttachment) {
		previous, err = "", nil
	}
	if err != nil {
		return nil, err
	}
	if previous != "" && previous != slug {
		if err := r.store.RequireSlug(slug); err != nil {
			return nil, err
		}
		if _, err := s.host.CloseDocsTabs(r.session.ID, previous); err != nil {
			return nil, fmt.Errorf("still attached to %s: closing its Docs tab failed: %w", previous, err)
		}
	}
	before, err := r.store.Head()
	if err != nil {
		return nil, err
	}
	if err := r.store.Attach(slug, r.session.ID); err != nil {
		return nil, err
	}
	s.publishSnapshot(r, before, "")
	info, err := s.summary(r, slug)
	if err != nil {
		return nil, err
	}
	info.Rules = Rules(slug, info.Path)
	tab, err := s.host.EnsureDocsTab(r.session.ID, slug)
	if err != nil {
		info.TabError = err.Error()
	} else {
		info.Tab = tab
	}
	return info, nil
}

func (s *Service) notAttached(r *resolved) error {
	slugs, _ := r.store.Slugs()
	if len(slugs) == 0 {
		return ErrNotAttached
	}
	return fmt.Errorf("%w; existing proposals: %s", ErrNotAttached, strings.Join(slugs, ", "))
}

// Detach removes the session's attachment and closes its Docs tab.
func (s *Service) Detach(sessionID string) (*DetachResult, error) {
	r, err := s.sessionTarget(sessionID)
	if err != nil {
		return nil, err
	}
	slug, err := r.store.Attached(r.session.ID)
	if errors.Is(err, errBadAttachment) {
		if _, err := r.store.Detach(r.session.ID); err != nil {
			return nil, err
		}
		return &DetachResult{Detached: true}, nil
	}
	if err != nil {
		return nil, err
	}
	res := &DetachResult{Slug: slug, Detached: slug != ""}
	if slug == "" {
		return res, nil
	}
	if res.Tab, err = s.host.CloseDocsTabs(r.session.ID, slug); err != nil {
		return nil, fmt.Errorf("still attached to %s: closing its Docs tab failed: %w", slug, err)
	}
	if _, err := r.store.Detach(r.session.ID); err != nil {
		return nil, err
	}
	return res, nil
}

// Changes returns the edits the session has not seen and moves its cursor.
func (s *Service) Changes(sessionID string) (*Changes, error) {
	r, err := s.sessionTarget(sessionID)
	if err != nil {
		return nil, err
	}
	defer s.markUnsentStale("", "", r.session.ID)
	before, err := r.store.Head()
	if err != nil {
		return nil, err
	}
	c, err := r.store.Changes(r.session.ID)
	if err != nil {
		return c, err
	}
	s.publishSnapshot(r, before, "")
	return c, nil
}

// publishSnapshot publishes the docs that the external snapshot a call took
// before its own work changed: the commits after before, up to the parent of
// own, the call's own commit, or up to HEAD when own is "".
func (s *Service) publishSnapshot(r *resolved, before, own string) {
	after, err := r.store.Head()
	if own != "" {
		after, err = r.store.gitTrim("rev-parse", "--verify", own+"^")
	}
	if err != nil || before == "" || after == before {
		return
	}
	docs, err := r.store.changedDocs(before, after)
	if err != nil {
		return
	}
	for _, d := range docs {
		s.committed(r, d.slug, d.file, after, AuthorExternal)
	}
}

// Checkpoint commits the session's edits to its proposal.
func (s *Service) Checkpoint(sessionID string) (*Checkpoint, error) {
	r, err := s.sessionTarget(sessionID)
	if err != nil {
		return nil, err
	}
	defer s.markUnsentStale("", "", r.session.ID)
	before, err := r.store.Head()
	if err != nil {
		return nil, err
	}
	cp, err := r.store.Checkpoint(r.session.ID)
	if err != nil {
		return cp, err
	}
	s.publishSnapshot(r, before, cp.Commit)
	if cp.Commit == "" {
		return cp, nil
	}
	files, _, err := r.store.commitDiff(cp.Commit, cp.Slug)
	if err != nil {
		return cp, nil
	}
	for _, f := range files {
		s.committed(r, cp.Slug, f, cp.Commit, AuthorAgent)
	}
	return cp, nil
}

func agentCommitter(r *resolved) Committer {
	return Committer{Author: AuthorAgent, SessionID: r.session.ID}
}

func committerFor(r *resolved, author Author) (Committer, error) {
	switch author {
	case AuthorAgent:
		if r.session == nil {
			return Committer{}, errors.New("author agent needs session_id")
		}
		return agentCommitter(r), nil
	case AuthorHuman, AuthorExternal:
		return Committer{Author: author}, nil
	case "":
		return Committer{}, errors.New("author is required: human, agent or external")
	}
	return Committer{}, fmt.Errorf("invalid author %q: use human, agent or external", author)
}

// AddDoc adds a doc to proposal slug.
func (s *Service) AddDoc(t Target, slug, name string, author Author) (*DocResult, error) {
	r, err := s.resolve(t)
	if err != nil {
		return nil, err
	}
	by, err := committerFor(r, author)
	if err != nil {
		return nil, err
	}
	before, err := r.store.Head()
	if err != nil {
		return nil, err
	}
	file, commit, err := r.store.AddDoc(slug, name, by)
	if err != nil {
		// The snapshot of other docs' disk edits may have landed before the add
		// failed; publish it so attached tabs do not go stale (#5594).
		s.publishSnapshot(r, before, "")
		return nil, err
	}
	s.publishSnapshot(r, before, commit)
	s.committed(r, slug, file, commit, author)
	return &DocResult{Slug: slug, Doc: file, Path: filepath.Join(r.store.SlugPath(slug), file), Commit: commit}, nil
}

// RemoveDoc deletes a doc from proposal slug.
func (s *Service) RemoveDoc(t Target, slug, doc string, author Author) (*DocResult, error) {
	r, err := s.resolve(t)
	if err != nil {
		return nil, err
	}
	by, err := committerFor(r, author)
	if err != nil {
		return nil, err
	}
	before, err := r.store.Head()
	if err != nil {
		return nil, err
	}
	file, commit, err := r.store.RemoveDoc(slug, doc, by)
	if err != nil {
		// The snapshot of other docs' disk edits may have landed before the
		// removal failed; publish it so attached tabs do not go stale (#5594).
		s.publishSnapshot(r, before, "")
		return nil, err
	}
	s.publishSnapshot(r, before, commit)
	s.committed(r, slug, file, commit, author)
	return &DocResult{Slug: slug, Doc: file, Path: filepath.Join(r.store.SlugPath(slug), file), Commit: commit}, nil
}

// Read returns a doc and the commit it is at.
func (s *Service) Read(t Target, slug, doc string) (*ReadResult, error) {
	r, err := s.resolve(t)
	if err != nil {
		return nil, err
	}
	text, file, commit, snapshotted, err := r.store.ReadAt(slug, doc)
	if err != nil {
		return nil, err
	}
	if snapshotted {
		s.committed(r, slug, file, commit, AuthorExternal)
	} else {
		s.seen(r, slug, file)
	}
	return &ReadResult{Slug: slug, Doc: file, Text: text, Commit: commit}, nil
}

// Write replaces a doc and commits it as author.
func (s *Service) Write(t Target, slug, doc, text string, author Author) (*DocResult, error) {
	r, err := s.resolve(t)
	if err != nil {
		return nil, err
	}
	by, err := committerFor(r, author)
	if err != nil {
		return nil, err
	}
	before, err := r.store.Head()
	if err != nil {
		return nil, err
	}
	file, commit, err := r.store.Write(slug, doc, text, by)
	if err != nil {
		s.publishSnapshot(r, before, "")
		return nil, err
	}
	s.publishSnapshot(r, before, commit)
	if commit != "" {
		s.committed(r, slug, file, commit, author)
	}
	return &DocResult{Slug: slug, Doc: file, Path: filepath.Join(r.store.SlugPath(slug), file), Commit: commit, Author: author}, nil
}

// Save is the Docs tab save: a human edit with a three-way merge against base.
func (s *Service) Save(t Target, slug, doc, text, base string) (*SaveDocResult, error) {
	return s.SaveFenced(t, slug, doc, text, base, "", 0)
}

// SaveFenced is Save with a writer fence. writer names one editor (a Docs tab's sync) and seq
// counts its saves upward: a save whose seq is not above the highest the daemon has seen from
// that writer for the doc is refused with a *StaleSaveError and commits nothing. That is what
// stops a request that outlived its client (a timeout, a closed tab) from landing after the
// writer's later saves: a three-way merge against base cannot, because the late edit merges
// cleanly. An empty writer or a zero seq is not fenced.
func (s *Service) SaveFenced(t Target, slug, doc, text, base, writer string, seq int64) (*SaveDocResult, error) {
	r, err := s.resolve(t)
	if err != nil {
		return nil, err
	}
	file := doc
	if n, err := NormalizeDocName(doc); err == nil {
		file = docFile(n)
	}
	if writer != "" && seq > 0 {
		key := r.repoID + "\x00" + slug + "\x00" + file + "\x00" + writer
		if ok, last := s.fence.admit(key, seq); !ok {
			return nil, &StaleSaveError{Writer: writer, Seq: seq, Last: last}
		}
	}
	before, err := r.store.Head()
	if err != nil {
		return nil, err
	}
	res, err := r.store.Save(slug, doc, text, base)
	if err != nil {
		var conflict *ConflictError
		if errors.As(err, &conflict) {
			if conflict.snapshotted {
				s.committed(r, slug, file, conflict.Versions.Head, AuthorExternal)
			}
		} else {
			s.publishSnapshot(r, before, "")
		}
		return nil, err
	}
	switch {
	case res.saved:
		s.committed(r, slug, file, res.Commit, AuthorHuman)
		s.noteHumanSave(r, slug)
	case res.snapshotted:
		s.committed(r, slug, file, res.Commit, AuthorExternal)
	default:
		s.seen(r, slug, file)
	}
	return &SaveDocResult{Slug: slug, Doc: file, SaveResult: *res}, nil
}
