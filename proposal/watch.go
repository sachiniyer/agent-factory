package proposal

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// WatchInput is what the daemon knows on each poll: the projects that have
// sessions, and the Docs tabs that are open, by project and slug.
type WatchInput struct {
	RepoIDs []string
	// Tabs maps repo id → slug → the ids of sessions with a Docs tab for it.
	Tabs map[string]map[string][]string
}

type fileStat struct {
	mod  time.Time
	size int64
}

type refsStamp struct {
	dir, packed fileStat
}

type attachCache struct {
	stamp refsStamp
	slugs map[string]string
}

type watchState struct {
	// files maps repo id → slug → doc file → the last stat seen.
	files   map[string]map[string]map[string]fileStat
	attachs map[string]attachCache
}

func newWatchState() watchState {
	return watchState{files: map[string]map[string]map[string]fileStat{}, attachs: map[string]attachCache{}}
}

func statOf(info fs.FileInfo) fileStat {
	return fileStat{mod: info.ModTime(), size: info.Size()}
}

func statPath(path string) fileStat {
	info, err := os.Stat(path)
	if err != nil {
		return fileStat{}
	}
	return statOf(info)
}

// Scan compares the docs of every watched proposal with the last scan and
// publishes a proposal.changed event for each doc that changed, appeared or
// went away. A proposal seen for the first time is recorded without events.
// Scan does not commit anything.
func (s *Service) Scan(in WatchInput) {
	repos := map[string]bool{}
	for _, id := range in.RepoIDs {
		repos[id] = true
	}
	for id := range in.Tabs {
		repos[id] = true
	}
	changes := s.scanLocked(repos, in.Tabs)
	for _, c := range changes {
		s.host.Publish(c)
	}
}

func (s *Service) scanLocked(repos map[string]bool, tabs map[string]map[string][]string) []Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSet = tabs
	var changes []Change
	for repoID := range s.watch.files {
		if !repos[repoID] {
			delete(s.watch.files, repoID)
			delete(s.watch.attachs, repoID)
		}
	}
	for repoID := range repos {
		store, err := Open(repoID)
		if err != nil || !store.initialized() {
			continue
		}
		watched := s.watchedSlugsLocked(store, repoID, tabs[repoID])
		known := s.watch.files[repoID]
		if known == nil {
			known = map[string]map[string]fileStat{}
			s.watch.files[repoID] = known
		}
		for slug := range known {
			if _, ok := watched[slug]; !ok {
				delete(known, slug)
			}
		}
		for slug, ids := range watched {
			current := docStats(store, slug)
			previous, seen := known[slug]
			known[slug] = current
			if !seen {
				continue
			}
			for _, file := range changedFiles(previous, current) {
				changes = append(changes, Change{
					RepoID: repoID, SessionIDs: ids, Slug: slug, Doc: file, Author: AuthorExternal,
				})
			}
		}
	}
	return changes
}

func changedFiles(previous, current map[string]fileStat) []string {
	var out []string
	for f, st := range current {
		if old, ok := previous[f]; !ok || !old.mod.Equal(st.mod) || old.size != st.size {
			out = append(out, f)
		}
	}
	for f := range previous {
		if _, ok := current[f]; !ok {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func docStats(store *Store, slug string) map[string]fileStat {
	out := map[string]fileStat{}
	entries, err := store.readSlugDir(slug)
	if err != nil {
		return out
	}
	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".md")
		if !e.Type().IsRegular() || !ok || !namePattern.MatchString(base) {
			continue
		}
		if info, err := e.Info(); err == nil {
			out[e.Name()] = statOf(info)
		}
	}
	return out
}

// watchedSlugsLocked maps each watched slug of a project to the sessions that
// watch it: the attached sessions and the sessions with a Docs tab for it.
func (s *Service) watchedSlugsLocked(store *Store, repoID string, tabs map[string][]string) map[string][]string {
	sets := map[string]map[string]bool{}
	add := func(slug, id string) {
		if sets[slug] == nil {
			sets[slug] = map[string]bool{}
		}
		sets[slug][id] = true
	}
	for id, slug := range s.attachmentsLocked(store, repoID) {
		add(slug, id)
	}
	for slug, ids := range tabs {
		for _, id := range ids {
			add(slug, id)
		}
	}
	out := map[string][]string{}
	for slug, set := range sets {
		ids := make([]string, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out[slug] = ids
	}
	return out
}

// attachmentsLocked reads the attach refs again only when the loose ref folder
// or packed-refs changed, so an idle poll costs two stats per project.
func (s *Service) attachmentsLocked(store *Store, repoID string) map[string]string {
	gitDir := store.gitDir()
	stamp := refsStamp{
		dir:    statPath(filepath.Join(gitDir, filepath.FromSlash(attachRefPrefix))),
		packed: statPath(filepath.Join(gitDir, "packed-refs")),
	}
	if c, ok := s.watch.attachs[repoID]; ok && c.stamp == stamp {
		return c.slugs
	}
	slugs, err := store.Attachments()
	if err != nil {
		return s.watch.attachs[repoID].slugs
	}
	s.watch.attachs[repoID] = attachCache{stamp: stamp, slugs: slugs}
	return slugs
}

// seen records the current stat of a doc the daemon itself wrote or read, so
// the next Scan does not report it as an edit on disk.
func (s *Service) seen(r *resolved, slug, file string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	known := s.watch.files[r.repoID][slug]
	if known == nil {
		return
	}
	info, err := r.store.statDoc(slug, file)
	if errors.Is(err, fs.ErrNotExist) {
		delete(known, file)
		return
	}
	if err == nil {
		known[file] = statOf(info)
	}
}

// committed records a doc the daemon committed and publishes the change.
func (s *Service) committed(r *resolved, slug, file, commit string, author Author) {
	s.seen(r, slug, file)
	ids, _ := s.attachedTo(r, slug)
	s.mu.Lock()
	for _, id := range s.lastSet[r.repoID][slug] {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	sort.Strings(ids)
	if ids == nil {
		ids = []string{}
	}
	c := commit
	s.host.Publish(Change{RepoID: r.repoID, SessionIDs: ids, Slug: slug, Doc: file, Commit: &c, Author: author})
}
