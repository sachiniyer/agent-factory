package proposal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/config"
)

// Author is the Af-Author trailer value of a commit.
type Author string

const (
	AuthorAgent    Author = "agent"
	AuthorHuman    Author = "human"
	AuthorExternal Author = "external"
)

// Store is the proposals repository of one project.
type Store struct {
	dir string
}

// Open returns the store of project repoID. It does not touch the disk; the
// repository is created on the first write.
func Open(repoID string) (*Store, error) {
	if err := config.ValidateRepoID(repoID); err != nil {
		return nil, err
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get config dir: %w", err)
	}
	return &Store{dir: filepath.Join(home, "repos", repoID, "proposals")}, nil
}

// Dir is the absolute path of the folder that holds the proposals.
func (s *Store) Dir() string { return s.dir }

// SlugPath is the absolute folder path of proposal slug.
func (s *Store) SlugPath(slug string) string { return filepath.Join(s.dir, slug) }

// withLock runs fn with the store's flock held and the repository initialized.
func (s *Store) withLock(fn func() error) error {
	if err := config.MkdirAllUnderAFHome(s.dir, 0o755); err != nil {
		return fmt.Errorf("create proposal store: %w", err)
	}
	dir, err := s.openStoreDir()
	if err != nil {
		return fmt.Errorf("open proposal store: %w", err)
	}
	dir.Close()
	f, err := os.OpenFile(s.dir+".lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return fmt.Errorf("open proposal store lock: %w", err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock proposal store: %w", err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	if err := s.ensureRepo(); err != nil {
		return fmt.Errorf("initialize proposal store: %w", err)
	}
	// A process that died mid-commit can leave entries staged; every commit
	// must start from HEAD so they are not swept into the wrong author's.
	if _, err := s.git(nil, "read-tree", "HEAD"); err != nil {
		return fmt.Errorf("reset proposal index: %w", err)
	}
	return fn()
}

// Slugs lists the proposals on disk, sorted by name.
func (s *Store) Slugs() ([]string, error) {
	dir, err := s.openStoreDir()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var slugs []string
	for _, e := range entries {
		if e.IsDir() && namePattern.MatchString(e.Name()) {
			slugs = append(slugs, e.Name())
		}
	}
	sort.Strings(slugs)
	return slugs, nil
}

// RequireSlug fails with the list of known slugs when slug does not exist.
func (s *Store) RequireSlug(slug string) error {
	if err := ValidateSlug(slug); err != nil {
		return err
	}
	if dir, err := s.openSlugDir(slug); err == nil {
		dir.Close()
		return nil
	}
	slugs, _ := s.Slugs()
	if len(slugs) == 0 {
		return fmt.Errorf("proposal %q not found: this project has no proposals; create one with af proposal start <slug>", slug)
	}
	return fmt.Errorf("proposal %q not found; existing proposals: %s", slug, strings.Join(slugs, ", "))
}

// Docs lists the doc files of proposal slug: built-in template names first in
// template order, then the rest alphabetically.
func (s *Store) Docs(slug string) ([]string, error) {
	entries, err := s.readSlugDir(slug)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".md")
		if e.Type().IsRegular() && ok && namePattern.MatchString(base) {
			names = append(names, base)
		}
	}
	rank := func(n string) int {
		if i := slices.Index(BuiltinTemplates, n); i >= 0 {
			return i
		}
		return len(BuiltinTemplates)
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := rank(names[i]), rank(names[j])
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	docs := make([]string, len(names))
	for i, n := range names {
		docs[i] = docFile(n)
	}
	return docs, nil
}

// LastEdit is the newest modification time of any doc in proposal slug.
func (s *Store) LastEdit(slug string) (time.Time, error) {
	var latest time.Time
	if dir, err := s.openSlugDir(slug); err == nil {
		if info, err := dir.Stat(); err == nil {
			latest = info.ModTime()
		}
		dir.Close()
	}
	docs, err := s.Docs(slug)
	if err != nil {
		return time.Time{}, err
	}
	for _, d := range docs {
		f, err := s.openDoc(slug, d)
		if err != nil {
			continue
		}
		if info, err := f.Stat(); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		f.Close()
	}
	return latest.UTC(), nil
}

// Committer names who a write is attributed to.
type Committer struct {
	Author    Author
	SessionID string
}

func (c Committer) validate() error {
	switch c.Author {
	case AuthorAgent:
		return validateSessionID(c.SessionID)
	case AuthorHuman, AuthorExternal:
		return nil
	}
	return fmt.Errorf("invalid author %q", c.Author)
}

// Create makes proposal slug with one doc per name, rendered from templates.
// It fails when the slug already exists.
func (s *Store) Create(slug string, docs []string, by Committer) error {
	if err := ValidateSlug(slug); err != nil {
		return err
	}
	if err := by.validate(); err != nil {
		return err
	}
	names, err := normalizeDocNames(docs)
	if err != nil {
		return err
	}
	return s.withLock(func() error {
		dir := s.SlugPath(slug)
		if _, err := os.Lstat(dir); err == nil {
			return fmt.Errorf("proposal %q already exists: run af proposal attach %s to work on it", slug, slug)
		}
		if _, err := s.snapshot(); err != nil {
			return err
		}
		if err := s.mkSlugDir(slug); err != nil {
			return fmt.Errorf("create proposal folder: %w", err)
		}
		if err := s.populate(slug, names, by); err != nil {
			_, _ = s.git(nil, "reset", "-q", "--", slug)
			if rmErr := s.removeSlugDir(slug); rmErr != nil {
				return fmt.Errorf("%w (and failed to remove %s: %v)", err, dir, rmErr)
			}
			return err
		}
		return nil
	})
}

func (s *Store) populate(slug string, names []string, by Committer) error {
	for _, n := range names {
		text, err := renderDoc(slug, n)
		if err != nil {
			return err
		}
		if err := s.createDoc(slug, docFile(n), []byte(text)); err != nil {
			return err
		}
	}
	_, err := s.commitPaths("start "+slug, by.Author, by.SessionID, slug)
	return err
}

func normalizeDocNames(docs []string) ([]string, error) {
	if len(docs) == 0 {
		return nil, errors.New("a proposal needs at least one doc")
	}
	var names []string
	for _, d := range docs {
		n, err := NormalizeDocName(strings.TrimSpace(d))
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names, nil
}

// AddDoc adds doc name to proposal slug from its template, or blank when no
// template has that name. It returns the doc file name and the new commit.
func (s *Store) AddDoc(slug, name string, by Committer) (string, string, error) {
	n, err := NormalizeDocName(name)
	if err != nil {
		return "", "", err
	}
	if err := by.validate(); err != nil {
		return "", "", err
	}
	var commit string
	err = s.withLock(func() error {
		if err := s.RequireSlug(slug); err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(s.SlugPath(slug), docFile(n))); err == nil {
			return fmt.Errorf("doc %s already exists in proposal %q", docFile(n), slug)
		}
		if _, err := s.snapshot(); err != nil {
			return err
		}
		text, err := renderDoc(slug, n)
		if err != nil {
			return err
		}
		if err := s.createDoc(slug, docFile(n), []byte(text)); err != nil {
			return err
		}
		commit, err = s.commitPaths("add "+slug+"/"+docFile(n), by.Author, by.SessionID, slug+"/"+docFile(n))
		if err != nil {
			if rmErr := s.removeDoc(slug, docFile(n)); rmErr != nil {
				return fmt.Errorf("%w (and failed to remove %s: %v)", err, docFile(n), rmErr)
			}
		}
		return err
	})
	return docFile(n), commit, err
}

// RemoveDoc deletes doc from proposal slug and commits the deletion, so the doc
// stays in the store's history. It returns the doc file name and the new commit.
func (s *Store) RemoveDoc(slug, doc string, by Committer) (string, string, error) {
	if err := by.validate(); err != nil {
		return "", "", err
	}
	var file, commit string
	err := s.withLock(func() error {
		_, f, err := s.docPath(slug, doc)
		if err != nil {
			return err
		}
		file = f
		if _, err := s.snapshot(); err != nil {
			return err
		}
		old, err := s.readDoc(slug, file)
		if err != nil {
			return err
		}
		if err := s.removeDoc(slug, file); err != nil {
			return err
		}
		commit, err = s.commitPaths("remove "+slug+"/"+file, by.Author, by.SessionID, slug+"/"+file)
		if err != nil {
			// Put the doc back so a retry is not snapshotted as an external delete.
			if rerr := s.createDoc(slug, file, old); rerr != nil {
				return fmt.Errorf("%w (and failed to restore %s: %v)", err, file, rerr)
			}
		}
		return err
	})
	return file, commit, err
}

// docPath validates slug and doc and returns the doc's absolute path and file
// name. The doc must be a regular file reached without following a link.
func (s *Store) docPath(slug, doc string) (string, string, error) {
	n, err := NormalizeDocName(doc)
	if err != nil {
		return "", "", err
	}
	if err := s.RequireSlug(slug); err != nil {
		return "", "", err
	}
	f, err := s.openDoc(slug, docFile(n))
	if err != nil {
		docs, _ := s.Docs(slug)
		return "", "", fmt.Errorf("doc %s not found in proposal %q; existing docs: %s", docFile(n), slug, strings.Join(docs, ", "))
	}
	f.Close()
	return filepath.Join(s.SlugPath(slug), docFile(n)), docFile(n), nil
}

// Read returns the text of an existing doc and its file name.
func (s *Store) Read(slug, doc string) (string, string, error) {
	_, file, err := s.docPath(slug, doc)
	if err != nil {
		return "", "", err
	}
	data, err := s.readDoc(slug, file)
	if err != nil {
		return "", "", err
	}
	return string(data), file, nil
}

// Write replaces an existing doc with text and commits it. It returns the doc
// file name and the new commit, or "" when the text did not change.
func (s *Store) Write(slug, doc, text string, by Committer) (string, string, error) {
	if err := by.validate(); err != nil {
		return "", "", err
	}
	var file, commit string
	err := s.withLock(func() error {
		_, f, err := s.docPath(slug, doc)
		if err != nil {
			return err
		}
		file = f
		if _, err := s.snapshot(); err != nil {
			return err
		}
		old, err := s.readDoc(slug, file)
		if err != nil {
			return err
		}
		if err := s.writeDoc(slug, file, []byte(text)); err != nil {
			return err
		}
		commit, err = s.commitPaths("write "+slug+"/"+file, by.Author, by.SessionID, slug+"/"+file)
		if err != nil {
			return s.restoreDoc(slug, file, old, err)
		}
		return nil
	})
	return file, commit, err
}

// restoreDoc puts back the text a failed commit was about to replace, so a
// retry is not snapshotted as an external edit.
func (s *Store) restoreDoc(slug, file string, old []byte, cause error) error {
	if err := s.writeDoc(slug, file, old); err != nil {
		return fmt.Errorf("%w (and failed to restore %s: %v)", cause, file, err)
	}
	return cause
}
