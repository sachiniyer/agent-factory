package app

import (
	"path/filepath"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// The project union's registry half (#7397 review R1): one registry read plus
// the canonical spelling of every path the union compares, taken in ONE bounded
// phase, and the per-path spelling cache that lets a read skip paths already
// resolved.
//
// Where each read runs is the other half of the property. The 750 ms sidebar
// poll reads its view OFF the loop, inside the snapshot fetch it already makes
// there, so a poll never waits on the registry's roots at all. A refresh the
// user's own action triggers (Ctrl+P, a switch, an add, a delete or a rebind
// completing) reads on the loop, because it must show that action's result at
// once; it holds Update for one bound at most, and pays nothing for a path the
// cache already has.

// projectRegistryView is one registry read and the spellings resolved with it.
type projectRegistryView struct {
	projects []config.Project
	err      error
	// spellings holds the canonical spelling of every registry root,
	// root_agents key and extra path the read covered.
	spellings pathSpellings
	// gen is the loop's registry generation when the read began. A read on the
	// loop advances it, so a poll that read the registry before that refresh
	// cannot replace the refresh's answer with an older one.
	gen uint64
}

// binding returns the record this view holds for id.
func (v *projectRegistryView) binding(id string) (config.Project, bool) {
	if v == nil || v.err != nil {
		return config.Project{}, false
	}
	for _, project := range v.projects {
		if project.ID == id {
			return project, true
		}
	}
	return config.Project{}, false
}

// readProjectRegistryView reads the registry and the spelling of every registry
// root, root_agents key and extra path in ONE concurrent phase: each root's
// stat, each root's resolve and every other resolve start together, so the read
// holds its caller for one bound in total, whatever the number of paths and
// however many are stalled. Each lookup still has a bound of its own, so a slow
// mount that answers inside it is read exactly as it answered (#7389 review
// R1's class). With revalidate every path is resolved again (the poll, off the
// loop); without it only the paths the cache has never seen are (a read on the
// loop).
func readProjectRegistryView(rootAgentKeys, extra []string, cache *projectSpellingCache, revalidate bool) *projectRegistryView {
	others := make([]string, 0, len(rootAgentKeys)+len(extra))
	others = append(append(others, rootAgentKeys...), extra...)
	otherSpellings := startResolvingSpellings(others, cache, revalidate)
	projects, rootSpellings, err := config.ListProjectsAlongside(func(root string) string {
		return cache.spellingFor(root, revalidate)
	})
	spellings := otherSpellings()
	view := &projectRegistryView{err: err, spellings: spellings}
	if err == nil {
		view.projects = projects
		for i, project := range projects {
			spellings[project.Root] = rootSpellings[i]
		}
	}
	return view
}

// rootAgentKeys is every root_agents key, expanded the way the union reads it.
func (m *home) rootAgentKeys() []string {
	if m.appConfig == nil {
		return nil
	}
	keys := make([]string, 0, len(m.appConfig.RootAgents))
	for path := range m.appConfig.RootAgents {
		keys = append(keys, config.ExpandTilde(path))
	}
	return keys
}

// spellingCache is the home's spelling cache, created on the loop at first use.
func (m *home) spellingCache() *projectSpellingCache {
	if m.projectSpellings == nil {
		m.projectSpellings = &projectSpellingCache{}
	}
	return m.projectSpellings
}

// readRegistryViewOnLoop is the registry read a refresh on the loop makes. It
// installs the view as the one the Projects section renders and advances the
// generation, so a poll whose read began before it is not applied over it.
func (m *home) readRegistryViewOnLoop(extra []string) *projectRegistryView {
	view := readProjectRegistryView(m.rootAgentKeys(), extra, m.spellingCache(), false)
	m.registryViewGen++
	view.gen = m.registryViewGen
	m.registryView = view
	return view
}

// registryViewForPoll is the view a poll's rows are built from: the one the poll
// read off the loop while it is still current; the loop's own newer read when a
// refresh superseded it; and a read on the loop only for a poll that carries no
// view at all (a message built by hand).
func (m *home) registryViewForPoll(polled *projectRegistryView) *projectRegistryView {
	switch {
	case polled != nil && polled.gen == m.registryViewGen:
		m.registryView = polled
		return polled
	case polled != nil && m.registryView != nil:
		return m.registryView
	default:
		return m.readRegistryViewOnLoop(nil)
	}
}

// projectSpellingCache keeps the last canonical spelling learned for each path
// the union compares. It is shared by the poll's off-loop read, which resolves
// every path it covers again each round and so follows a changed symlink within
// a poll, and the loop's reads, which resolve only paths it has never seen: a
// hit costs Update nothing, so a slow but answering mount does not cost it its
// latency on every read. A non-answer never replaces an answered spelling —
// a stalled mount does not move its symlinks — and an answer always does.
type projectSpellingCache struct {
	mu     sync.Mutex
	byPath map[string]cachedSpelling
}

type cachedSpelling struct {
	spelling string
	answered bool
}

// spellingFor returns path's spelling: the cached one unless revalidate or the
// cache has none, else a bounded resolve whose outcome the cache records.
func (c *projectSpellingCache) spellingFor(path string, revalidate bool) string {
	if !revalidate {
		if spelling, ok := c.lookup(path); ok {
			return spelling
		}
	}
	spelling, answered := resolveForCompare(path)
	return c.record(path, spelling, answered)
}

func (c *projectSpellingCache) lookup(path string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.byPath[path]
	return cached.spelling, ok
}

// record stores a resolve's outcome and returns the spelling to use: the
// answer, or on a non-answer the spelling already held (else the cleaned path,
// which a non-answer falls back to anyway).
func (c *projectSpellingCache) record(path, spelling string, answered bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byPath == nil {
		c.byPath = make(map[string]cachedSpelling)
	}
	if cached, ok := c.byPath[path]; ok && !answered {
		return cached.spelling
	}
	c.byPath[path] = cachedSpelling{spelling: spelling, answered: answered}
	return spelling
}

// retain forgets every path outside covered, so the cache holds what the
// latest poll compares and does not grow with paths nothing names any more.
func (c *projectSpellingCache) retain(covered pathSpellings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for path := range c.byPath {
		if _, ok := covered[path]; !ok {
			delete(c.byPath, path)
		}
	}
}

// resolveForCompare is the bounded, event-loop-safe spelling canonicalizer for
// path-identity comparisons: pathutil.ResolveForCompare inside one fsprobe
// flight (#7272). answered is false on a non-answer, and the spelling is then
// the cleaned path, so a wedged mount degrades the comparison to Clean-vs-Clean
// instead of hanging the loop forever.
//
// An on-loop caller must never invoke pathutil.ResolveForCompare directly: its
// ancestor walk is unbounded, and on a stalled NFS/FUSE/sshfs mount the first
// EvalSymlinks never returns — parking Update, key input, repaint and the
// Ctrl+C hard exit until the process is SIGKILLed.
//
// The bound is per CALL, not per read. One call waits at most one flight bound
// (2 s in production, overridable in tests) for its path, and a call made while
// that path's earlier flight is still stuck is refused at once. A read that
// resolves N paths one call after another therefore waits up to N bounds — ten
// roots on one dead mount park the loop for about 20 s. A read resolving more
// than one path starts them together (readProjectRegistryView), so it holds for
// one bound in total, and skips the ones the cache holds (#7397 review R1).
func resolveForCompare(path string) (spelling string, answered bool) {
	cleaned := filepath.Clean(path)
	resolved, err := fsprobe.ResolveForCompare(cleaned)
	if err != nil {
		return cleaned, false
	}
	return resolved, true
}

// pathSpellings maps each path to its canonical spelling.
type pathSpellings map[string]string

// same compares two roots under the #2110 spelling rule; an empty side never
// matches. A path the read did not cover is resolved on its own, still bounded,
// so a caller that missed one pays one more bound rather than a wrong answer.
func (s pathSpellings) same(a, b string) bool {
	return a != "" && b != "" && s.canonical(a) == s.canonical(b)
}

func (s pathSpellings) canonical(path string) string {
	if spelling, ok := s[path]; ok {
		return spelling
	}
	spelling, _ := resolveForCompare(path)
	return spelling
}

// startResolvingSpellings starts the spelling of every distinct non-empty path
// at once — a cache hit at once, a resolve on its own goroutine — and returns
// the wait that collects them. Splitting the start from the wait is what lets
// readProjectRegistryView overlap these resolves with the registry read.
func startResolvingSpellings(paths []string, cache *projectSpellingCache, revalidate bool) func() pathSpellings {
	spellings := make(pathSpellings, len(paths))
	started := make(map[string]bool, len(paths))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, path := range paths {
		if path == "" || started[path] {
			continue
		}
		started[path] = true
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			spelling := cache.spellingFor(path, revalidate)
			mu.Lock()
			spellings[path] = spelling
			mu.Unlock()
		}(path)
	}
	return func() pathSpellings {
		wg.Wait()
		return spellings
	}
}
