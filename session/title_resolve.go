package session

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrAmbiguousTitle marks a bare-title lookup that matched sessions in more than
// one repo. Session titles are unique PER-REPO, not globally, so a bare title
// with no repo scope (no --repo, cwd outside a repo) can legitimately name more
// than one session. Resolving it by picking the first match is worse than
// failing: the all-repo scans iterate a Go map, so the "winner" is
// nondeterministic across runs, and a destructive command (kill/archive) could
// hit a different repo's session than the one the user meant.
//
// Callers match it with errors.Is and build the user-facing message with
// AmbiguousTitleError. A title matching exactly ONE session globally still
// resolves — the convenience of a bare title is kept for the common case.
var ErrAmbiguousTitle = errors.New("ambiguous session title")

// ambiguousTitleError names the repos holding a duplicated title. It is a type
// rather than a fmt.Errorf(%w) wrapper so the message ends at the actionable
// advice: wrapping would append the sentinel's own text, giving the user a
// message with a redundant "...pass --repo to pick one: ambiguous session
// title" tail. Unwrap keeps errors.Is(err, ErrAmbiguousTitle) working.
type ambiguousTitleError struct {
	title string
	repos []string
}

func (e *ambiguousTitleError) Error() string {
	if len(e.repos) == 0 {
		// Defensive: several matches but no repo could be named (e.g. records
		// with an empty Path). Still refuse to guess, and still point at the
		// flag that resolves it.
		return fmt.Sprintf("session %q exists in multiple projects — pass --repo to pick one", e.title)
	}
	return fmt.Sprintf("session %q exists in multiple projects: %s — pass --repo to pick one",
		e.title, strings.Join(e.repos, ", "))
}

func (e *ambiguousTitleError) Unwrap() error { return ErrAmbiguousTitle }

// AmbiguousTitleError builds the user-facing error for a bare title that matches
// sessions in several repos, naming each repo so the user can pick one. Paths are
// sorted and de-duplicated so the message is stable across runs (the underlying
// scans walk a map in nondeterministic order).
func AmbiguousTitleError(title string, repoPaths []string) error {
	return &ambiguousTitleError{title: title, repos: DedupeSorted(repoPaths)}
}

// DedupeSorted returns the distinct non-empty entries of in, sorted. Callers use
// it to collapse per-session matches down to the set of repos that hold the
// title: several sessions can share one repo path, and the scans that feed it
// walk a map, so the result must be de-duplicated and ordered to be stable.
func DedupeSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
