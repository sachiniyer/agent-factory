package proposal

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// ErrorCodeConflict is the envelope error code of a save that conflicts.
const ErrorCodeConflict = "proposal_conflict"

// commitIDPattern is the only base Save accepts. base reaches git rev-parse, so
// a ref name or a value that starts with '-' must never get there.
var commitIDPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// SaveResult is the result of Store.Save.
type SaveResult struct {
	Text   string `json:"text"`
	Commit string `json:"commit"`
	Merged bool   `json:"merged"`
	// saved is true when the save made a human commit.
	saved bool
	// snapshotted is true when the save first committed an external edit.
	snapshotted bool
}

// ConflictData is what a conflicting save sends back: the three versions and
// the head they were read at.
type ConflictData struct {
	Ours   string `json:"ours"`
	Theirs string `json:"theirs"`
	Base   string `json:"base"`
	Head   string `json:"head"`
}

// ConflictError means the doc changed since the save's base and git merge-file
// could not merge the two edits. The save commits nothing, but an external
// edit on disk may have been committed first; then snapshotted is true and
// Versions.Head is that commit.
type ConflictError struct {
	File        string
	BaseRef     string
	Versions    ConflictData
	snapshotted bool
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s changed since base %s, and the edits conflict", e.File, shortCommit(e.BaseRef))
}

// APIErrorCode, HTTPStatus and EnvelopeData shape the daemon's HTTP reply.
func (e *ConflictError) APIErrorCode() string { return ErrorCodeConflict }
func (e *ConflictError) HTTPStatus() int      { return http.StatusConflict }
func (e *ConflictError) EnvelopeData() any    { return e.Versions }

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// Save writes text to an existing doc as a human edit. base is the commit the
// editor read the doc at. When the doc changed since base, Save runs a
// three-way merge: a clean merge commits the merged text, and a conflict
// commits nothing and returns a *ConflictError.
func (s *Store) Save(slug, doc, text, base string) (*SaveResult, error) {
	return s.SaveGuarded(slug, doc, text, base, nil)
}

// SaveGuarded is Save held to guard (nil for none): guard.Admit runs under the store's lock
// before anything is written, and guard.Committed runs under the same lock once the save has
// succeeded.
func (s *Store) SaveGuarded(slug, doc, text, base string, guard SaveGuard) (*SaveResult, error) {
	if strings.TrimSpace(base) == "" {
		return nil, errors.New("base is required: send the commit from the last read or save of this doc")
	}
	if !commitIDPattern.MatchString(base) {
		return nil, fmt.Errorf("unknown base commit %q: read the doc again", base)
	}
	var res *SaveResult
	err := s.withLock(func() error {
		if guard != nil {
			if err := guard.Admit(); err != nil {
				return err
			}
		}
		_, file, err := s.docPath(slug, doc)
		if err != nil {
			return err
		}
		rel := slug + "/" + file
		baseCommit, err := s.gitTrim("rev-parse", "--verify", "--quiet", base+"^{commit}")
		if err != nil {
			return fmt.Errorf("unknown base commit %q: read the doc again", base)
		}
		snap, err := s.commitPaths("snapshot external edits", AuthorExternal, "", rel)
		if err != nil {
			return err
		}
		head, err := s.head()
		if err != nil {
			return err
		}
		baseText, err := s.fileAt(baseCommit, rel)
		if err != nil {
			return err
		}
		theirs, err := s.fileAt(head, rel)
		if err != nil {
			return err
		}
		data := []byte(theirs)
		next, merged := text, false
		if theirs != baseText && theirs != text {
			out, clean, err := s.mergeFile(text, baseText, theirs)
			if err != nil {
				return err
			}
			if !clean {
				return &ConflictError{File: file, BaseRef: baseCommit, Versions: ConflictData{
					Ours: text, Theirs: theirs, Base: baseText, Head: head,
				}, snapshotted: snap != ""}
			}
			next, merged = out, true
		}
		commit := head
		if next != theirs {
			if err := s.writeDoc(slug, file, []byte(next)); err != nil {
				return err
			}
			c, err := s.commitPaths("save "+rel, AuthorHuman, "", rel)
			if err != nil {
				return s.restoreDoc(slug, file, data, err)
			}
			if c != "" {
				commit = c
			}
		}
		res = &SaveResult{Text: next, Commit: commit, Merged: merged, saved: commit != head, snapshotted: snap != ""}
		if guard != nil {
			guard.Committed()
		}
		return nil
	})
	return res, err
}

// fileAt returns the text of rel at commit, or "" when rel does not exist there.
func (s *Store) fileAt(commit, rel string) (string, error) {
	out, err := s.gitTrim("ls-tree", "--name-only", commit, "--", rel)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	return s.git(nil, "cat-file", "blob", commit+":"+rel)
}

// mergeFile runs git merge-file on the three texts. clean is false when the
// merge has conflicts.
func (s *Store) mergeFile(ours, base, theirs string) (merged string, clean bool, err error) {
	var paths []string
	defer func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}()
	for _, text := range []string{ours, base, theirs} {
		f, err := os.CreateTemp("", "af-proposal-merge-*")
		if err != nil {
			return "", false, err
		}
		paths = append(paths, f.Name())
		_, werr := f.WriteString(text)
		cerr := f.Close()
		if err := errors.Join(werr, cerr); err != nil {
			return "", false, err
		}
	}
	out, err := s.gitOutput(nil, "merge-file", "-p", "-L", "ours", "-L", "base", "-L", "theirs", paths[0], paths[1], paths[2])
	if err == nil {
		return out, true, nil
	}
	// merge-file exits with the number of conflicts, capped at 127; a higher
	// status is a failure.
	var code int
	if exitCode(err, &code) && code > 0 && code < 128 {
		return out, false, nil
	}
	return "", false, err
}
