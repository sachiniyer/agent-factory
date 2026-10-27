package proposal

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	attachRefPrefix = "refs/af/attach/"
	cursorRefPrefix = "refs/af/cursor/"
)

// ErrNotAttached means the session has no proposal.
var ErrNotAttached = errors.New("this session is not attached to a proposal: run af proposal start <slug> or af proposal attach <slug>")

// Attached returns the slug that session sessionID is attached to, or "" when
// it has none.
func (s *Store) Attached(sessionID string) (string, error) {
	if err := validateSessionID(sessionID); err != nil {
		return "", err
	}
	if ok, err := s.present(); !ok || err != nil {
		return "", err
	}
	return s.attachedLocked(sessionID)
}

func (s *Store) attachedLocked(sessionID string) (string, error) {
	out, err := s.git(nil, "cat-file", "blob", attachRefPrefix+sessionID)
	if err != nil {
		if _, verr := s.git(nil, "rev-parse", "--verify", "--quiet", attachRefPrefix+sessionID); verr != nil {
			return "", nil
		}
		if kind, terr := s.git(nil, "cat-file", "-t", attachRefPrefix+sessionID); terr != nil || strings.TrimSpace(kind) != "blob" {
			return "", fmt.Errorf("%w: session %s: the ref names no readable blob", errBadAttachment, sessionID)
		}
		return "", err
	}
	slug := strings.TrimSpace(out)
	if err := ValidateSlug(slug); err != nil {
		return "", fmt.Errorf("%w: session %s: %v", errBadAttachment, sessionID, err)
	}
	return slug, nil
}

// errBadAttachment marks an attachment ref whose blob names no valid slug.
// Attach and Detach replace or drop such a ref; everything else refuses it.
var errBadAttachment = errors.New("corrupt proposal attachment; run af proposal attach or detach to repair it")

// Attachments maps each attached session ID to its slug.
func (s *Store) Attachments() (map[string]string, error) {
	got := map[string]string{}
	if ok, err := s.present(); !ok || err != nil {
		return got, err
	}
	out, err := s.git(nil, "for-each-ref", "--format=%(refname)", attachRefPrefix)
	if err != nil {
		return nil, err
	}
	for _, ref := range strings.Fields(out) {
		id := strings.TrimPrefix(ref, attachRefPrefix)
		slug, err := s.attachedLocked(id)
		if errors.Is(err, errBadAttachment) {
			continue
		}
		if err != nil {
			return nil, err
		}
		got[id] = slug
	}
	return got, nil
}

// Attach attaches session sessionID to proposal slug. A session holds at most
// one proposal, so this replaces an earlier attachment. Attaching again to the
// same slug keeps the cursor, so no unread change is skipped.
func (s *Store) Attach(slug, sessionID string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	return s.withLock(func() error {
		if err := s.RequireSlug(slug); err != nil {
			return err
		}
		current, err := s.attachedLocked(sessionID)
		if errors.Is(err, errBadAttachment) {
			current, err = "", nil
		}
		if err != nil {
			return err
		}
		if current == slug {
			return s.ensureCursor(sessionID)
		}
		if _, err := s.snapshot(); err != nil {
			return err
		}
		blob, err := s.git([]byte(slug+"\n"), "hash-object", "-w", "--stdin")
		if err != nil {
			return err
		}
		head, err := s.head()
		if err != nil {
			return err
		}
		tx := "update " + attachRefPrefix + sessionID + " " + strings.TrimSpace(blob) + "\n" +
			"update " + cursorRefPrefix + sessionID + " " + head + "\n"
		_, err = s.git([]byte(tx), "update-ref", "--stdin")
		return err
	})
}

// ensureCursor creates the session's cursor at HEAD when it is missing, for
// example after a crash between the attach and cursor ref updates.
func (s *Store) ensureCursor(sessionID string) error {
	if _, err := s.git(nil, "rev-parse", "--verify", "--quiet", cursorRefPrefix+sessionID+"^{commit}"); err == nil {
		return nil
	} else if !isExitCode(err, 1) {
		return err
	}
	head, err := s.head()
	if err != nil {
		return err
	}
	_, err = s.git(nil, "update-ref", cursorRefPrefix+sessionID, head)
	return err
}

// Detach removes the attachment and cursor of session sessionID. It returns
// the slug it was attached to, or "" when it had none.
func (s *Store) Detach(sessionID string) (string, error) {
	if err := validateSessionID(sessionID); err != nil {
		return "", err
	}
	if ok, err := s.present(); !ok || err != nil {
		return "", err
	}
	var slug string
	err := s.withLock(func() error {
		var err error
		if slug, err = s.attachedLocked(sessionID); errors.Is(err, errBadAttachment) {
			slug, err = "", nil
		}
		if err != nil {
			return err
		}
		for _, ref := range []string{attachRefPrefix + sessionID, cursorRefPrefix + sessionID} {
			if _, err := s.git(nil, "update-ref", "-d", ref); err != nil {
				return err
			}
		}
		return nil
	})
	return slug, err
}

// Edit is one commit that a session has not seen.
type Edit struct {
	Commit  string   `json:"commit"`
	Author  Author   `json:"author"`
	Session string   `json:"session,omitempty"`
	At      string   `json:"at"`
	Files   []string `json:"files"`
	Patch   string   `json:"patch"`
}

// Changes is the result of Store.Changes.
type Changes struct {
	Slug  string `json:"slug"`
	From  string `json:"from"`
	To    string `json:"to"`
	Edits []Edit `json:"edits"`
}

// Changes snapshots external edits, returns every commit since the session's
// cursor that touches its proposal and is not its own agent commit, and moves
// the cursor to HEAD.
func (s *Store) Changes(sessionID string) (*Changes, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	if ok, err := s.present(); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNotAttached
	}
	var res *Changes
	err := s.withLock(func() error {
		slug, cursor, err := s.sessionState(sessionID)
		if err != nil {
			return err
		}
		if _, err := s.snapshot(); err != nil {
			return err
		}
		head, err := s.head()
		if err != nil {
			return err
		}
		edits, err := s.unseen(slug, sessionID, cursor, head)
		if err != nil {
			return err
		}
		if _, err := s.git(nil, "update-ref", cursorRefPrefix+sessionID, head); err != nil {
			return err
		}
		res = &Changes{Slug: slug, From: cursor, To: head, Edits: edits}
		return nil
	})
	return res, err
}

// Checkpoint is the result of Store.Checkpoint.
type Checkpoint struct {
	Slug   string `json:"slug"`
	Commit string `json:"commit"`
	Cursor string `json:"cursor"`
	// Unseen counts edits by others that changes has not shown yet. The
	// cursor does not move past them.
	Unseen int `json:"unseen"`
}

// Checkpoint commits the uncommitted edits in the session's proposal as its
// own. Edits outside that proposal are snapshotted as external first. The
// cursor moves to HEAD only when that skips no edit by someone else.
func (s *Store) Checkpoint(sessionID string) (*Checkpoint, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	if ok, err := s.present(); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNotAttached
	}
	var res *Checkpoint
	err := s.withLock(func() error {
		slug, cursor, err := s.sessionState(sessionID)
		if err != nil {
			return err
		}
		if _, err := s.snapshot(slug); err != nil {
			return err
		}
		commit, err := s.commitPaths("checkpoint "+slug, AuthorAgent, sessionID, slug)
		if err != nil {
			return err
		}
		head, err := s.head()
		if err != nil {
			return err
		}
		edits, err := s.unseen(slug, sessionID, cursor, head)
		if err != nil {
			return err
		}
		if len(edits) == 0 {
			if _, err := s.git(nil, "update-ref", cursorRefPrefix+sessionID, head); err != nil {
				return err
			}
			cursor = head
		}
		res = &Checkpoint{Slug: slug, Commit: commit, Cursor: cursor, Unseen: len(edits)}
		return nil
	})
	return res, err
}

func (s *Store) sessionState(sessionID string) (slug, cursor string, err error) {
	slug, err = s.attachedLocked(sessionID)
	if err != nil {
		return "", "", err
	}
	if slug == "" {
		return "", "", ErrNotAttached
	}
	cursor, err = s.gitTrim("rev-parse", "--verify", cursorRefPrefix+sessionID+"^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("proposal cursor of this session is missing: run af proposal attach %s: %w", slug, err)
	}
	return slug, cursor, nil
}

const (
	fieldSep  = "\x1f"
	recordSep = "\x1e"
)

// unseen lists commits in from..to that touch slug, oldest first, without the
// agent commits of session sessionID.
func (s *Store) unseen(slug, sessionID, from, to string) ([]Edit, error) {
	format := "--format=%H" + fieldSep + "%ct" + fieldSep +
		"%(trailers:key=Af-Author,valueonly,separator=)" + fieldSep +
		"%(trailers:key=Af-Session,valueonly,separator=)" + recordSep
	out, err := s.git(nil, "log", "--reverse", format, from+".."+to, "--", slug+"/")
	if err != nil {
		return nil, err
	}
	edits := []Edit{}
	for _, rec := range strings.Split(out, recordSep) {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		f := strings.Split(rec, fieldSep)
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected git log record %q", rec)
		}
		e := Edit{
			Commit:  f[0],
			Author:  Author(strings.TrimSpace(f[2])),
			Session: strings.TrimSpace(f[3]),
		}
		if e.Author == AuthorAgent && e.Session == sessionID {
			continue
		}
		if e.Author == "" {
			e.Author = AuthorExternal
		}
		if e.Author != AuthorAgent {
			e.Session = ""
		}
		if sec, err := strconv.ParseInt(f[1], 10, 64); err == nil {
			e.At = time.Unix(sec, 0).UTC().Format(time.RFC3339)
		}
		if e.Files, e.Patch, err = s.commitDiff(e.Commit, slug); err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	return edits, nil
}

func (s *Store) commitDiff(commit, slug string) ([]string, string, error) {
	rel := "--relative=" + slug + "/"
	names, err := s.git(nil, "show", "--format=", "--name-only", rel, commit)
	if err != nil {
		return nil, "", err
	}
	patch, err := s.git(nil, "show", "--format=", "--patch", "--text", "--no-ext-diff", "--no-textconv", rel, commit)
	if err != nil {
		return nil, "", err
	}
	files := []string{}
	for _, line := range strings.Split(names, "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, patch, nil
}

func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}
