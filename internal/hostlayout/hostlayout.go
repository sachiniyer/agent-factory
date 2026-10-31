// Package hostlayout keeps the set of strings that describe the daemon HOST's
// layout (project paths, worktree paths, tmux names, home directories) and
// withholds them from a body in one pass (#5571 W3.5).
//
// It exists so the scrub on a sandbox credential's responses costs about one
// pass over the body no matter how many sessions the daemon tracks, and never
// takes another session's lock. The layout is MAINTAINED, not recomputed per
// request: the places that produce layout strings (an instance's projection or
// restore, the task store, the project registry) Note them as they pass through,
// so a string exists in the set before any response can contain it, and one
// compiled replacer is built per layout generation — invalidated by a change to
// the set, never by a timer.
package hostlayout

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// Withheld replaces every layout string found in a body.
const Withheld = "[daemon host path withheld]"

// Index is a set of layout strings with a compiled replacer per generation.
type Index struct {
	mu    sync.RWMutex
	seen  map[string]struct{}
	gen   uint64
	built uint64
	rep   *strings.Replacer
}

// Default is the process-wide index: one daemon per process, and a string noted
// by any producer is withheld from every sandbox response.
var Default = New()

// New returns an empty index.
func New() *Index { return &Index{seen: map[string]struct{}{}} }

// normalize drops what is too short to be a path or name worth withholding
// ("", "/", "ab"): replacing those would shred ordinary text.
func normalize(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if len(s) < 4 {
		return ""
	}
	return s
}

// Note adds layout strings. The common case (everything already known) takes
// only the read lock.
func (x *Index) Note(strs ...string) {
	x.mu.RLock()
	fresh := false
	for _, s := range strs {
		if n := normalize(s); n != "" {
			if _, ok := x.seen[n]; !ok {
				fresh = true
				break
			}
		}
	}
	x.mu.RUnlock()
	if !fresh {
		return
	}
	x.mu.Lock()
	for _, s := range strs {
		n := normalize(s)
		if n == "" {
			continue
		}
		if _, ok := x.seen[n]; !ok {
			x.seen[n] = struct{}{}
			x.gen++
		}
	}
	x.mu.Unlock()
}

// Len reports how many distinct strings are held.
func (x *Index) Len() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.seen)
}

// Scrub returns body with every noted string withheld, in one pass.
func (x *Index) Scrub(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	r := x.replacer()
	if r == nil {
		return body
	}
	return []byte(r.Replace(string(body)))
}

// ScrubString is Scrub for one string.
func (x *Index) ScrubString(s string) string {
	r := x.replacer()
	if r == nil || s == "" {
		return s
	}
	return r.Replace(s)
}

// replacer returns the compiled replacer for the current generation, building it
// when the set changed since the last one.
func (x *Index) replacer() *strings.Replacer {
	x.mu.RLock()
	if x.built == x.gen {
		r := x.rep
		x.mu.RUnlock()
		return r
	}
	x.mu.RUnlock()
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.built == x.gen {
		return x.rep
	}
	// Every spelling is a pattern: the raw string and, when it differs, its
	// JSON-escaped form (the body is JSON: a path with '&', '<', a backslash or
	// non-ASCII is spelled escaped). ALL of them are sorted longest first, as one
	// list. A replacer takes the first pattern that matches in argument order, so
	// an escaped spelling placed after its own raw form lost to it whenever the
	// raw string is a prefix of the escaped one — a path ending in a backslash
	// ("/srv/project\\") matches the front of "/srv/project\\\\" and leaves a dangling
	// escape. Sorting the whole list puts the longer, escaped spelling first.
	pats := make([]string, 0, 2*len(x.seen))
	for s := range x.seen {
		pats = append(pats, s)
		if esc, err := json.Marshal(s); err == nil && len(esc) > 2 {
			if inner := string(esc[1 : len(esc)-1]); inner != s {
				pats = append(pats, inner)
			}
		}
	}
	sort.Slice(pats, func(i, j int) bool {
		if len(pats[i]) != len(pats[j]) {
			return len(pats[i]) > len(pats[j])
		}
		return pats[i] < pats[j]
	})
	pairs := make([]string, 0, 2*len(pats))
	for _, p := range pats {
		pairs = append(pairs, p, Withheld)
	}
	if len(pairs) == 0 {
		x.rep = nil
	} else {
		x.rep = strings.NewReplacer(pairs...)
	}
	x.built = x.gen
	return x.rep
}
