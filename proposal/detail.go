package proposal

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// DocRef names one doc of a proposal.
type DocRef struct {
	Name string `json:"name"`
	File string `json:"file"`
}

// Detail is what the Docs tab needs to draw a proposal.
type Detail struct {
	Slug string   `json:"slug"`
	Path string   `json:"path"`
	Head string   `json:"head"`
	Docs []DocRef `json:"docs"`
	// Templates are the template names the proposal does not use yet.
	Templates        []string `json:"templates"`
	OpenQuestions    int      `json:"open_questions"`
	AttachedSessions []string `json:"attached_sessions"`
}

var questionCallout = regexp.MustCompile(`(?im)^[ \t]*>[ \t]*\[!question\]`)

// CountOpenQuestions counts the "> [!question]" callouts in text.
func CountOpenQuestions(text string) int {
	return len(questionCallout.FindAllStringIndex(text, -1))
}

// Head returns the store's HEAD commit, or "" when the store has no repository
// yet.
func (s *Store) Head() (string, error) {
	if !s.initialized() {
		return "", nil
	}
	return s.head()
}

// Detail describes proposal slug. AttachedSessions is left for the caller,
// which knows the session titles.
func (s *Store) Detail(slug string) (*Detail, error) {
	if err := s.RequireSlug(slug); err != nil {
		return nil, err
	}
	files, err := s.Docs(slug)
	if err != nil {
		return nil, err
	}
	head, err := s.Head()
	if err != nil {
		return nil, err
	}
	d := &Detail{Slug: slug, Path: s.SlugPath(slug), Head: head, Docs: []DocRef{}, AttachedSessions: []string{}}
	used := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(f, ".md")
		used[name] = true
		d.Docs = append(d.Docs, DocRef{Name: name, File: f})
		data, err := s.readDoc(slug, f)
		if err != nil {
			return nil, err
		}
		d.OpenQuestions += CountOpenQuestions(string(data))
	}
	names, err := TemplateNames()
	if err != nil {
		return nil, err
	}
	d.Templates = []string{}
	for _, n := range names {
		if !used[n] {
			d.Templates = append(d.Templates, n)
		}
	}
	return d, nil
}

// TemplateNames lists every template name: the built-in ones in display order,
// then user templates that are not built in, alphabetically.
func TemplateNames() ([]string, error) {
	names := append([]string(nil), BuiltinTemplates...)
	dir, err := userTemplateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var extra []string
	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".md")
		if e.IsDir() || !ok || !namePattern.MatchString(base) || slices.Contains(names, base) {
			continue
		}
		extra = append(extra, base)
	}
	sort.Strings(extra)
	return append(names, extra...), nil
}

// ReadAt returns an existing doc together with the commit it is at. It first
// commits uncommitted edits of that doc as external, so the text is exactly the
// doc at the returned commit and can serve as the base of a later Save.
// snapshotted is true when it made that external commit.
func (s *Store) ReadAt(slug, doc string) (text, file, commit string, snapshotted bool, err error) {
	err = s.withLock(func() error {
		_, f, err := s.docPath(slug, doc)
		if err != nil {
			return err
		}
		file = f
		snap, err := s.commitPaths("snapshot external edits", AuthorExternal, "", slug+"/"+f)
		if err != nil {
			return err
		}
		snapshotted = snap != ""
		if commit, err = s.head(); err != nil {
			return err
		}
		text, err = s.fileAt(commit, slug+"/"+f)
		return err
	})
	return text, file, commit, snapshotted, err
}
