package bugreport

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// The path-root policy: how a bundle names a directory.
//
// It is separated from redact.go because it is the one redaction rule that has
// to hold across every section at once — the JSON records, the daemon log tail,
// the archive warning, the task projection, the daemon status block — and a
// policy stated in one place cannot drift between them (#3588).

// Root tokens. A bundle names every directory it mentions by the ROLE that
// directory plays, never by its own name: "[repo:1]", "[worktree:2]",
// "[af-home]". Numbered per distinct root, so two sessions in one repo read as
// being in one repo.
//
// They exist because $HOME is not a guarantee. scrub replaces r.home and the
// username tokens and NOTHING else, so a repo or worktree deliberately kept
// outside the home directory — /srv/ConfidentialClient/repo, a sibling checkout,
// anything reached via --repo — shipped its directory names verbatim, which are
// exactly as revealing as the file names #3541 was about (#3588).
//
// A TOKEN rather than the marker, because triage needs the layout: whether a
// worktree sits under the AF home, whether a relocation alternate is its
// sibling, whether two sessions share a repo. Blanking the whole path destroys
// all three.
const (
	repoRootTokenFormat     = "[repo:%d]"
	worktreeRootTokenFormat = "[worktree:%d]"
	afHomeToken             = "[af-home]"
)

// pathRoot is one absolute directory this run can name, and the token that
// stands in for it everywhere the bundle mentions it.
type pathRoot struct {
	path  string
	token string
}

// noteRepoRoot registers one repository root, and noteWorktreeRoot one worktree
// root, under the next token of that kind.
func (r *redactor) noteRepoRoot(path string) {
	if r.noteRoot(path, fmt.Sprintf(repoRootTokenFormat, r.repoRoots+1)) {
		r.repoRoots++
	}
}

func (r *redactor) noteWorktreeRoot(path string) {
	if r.noteRoot(path, fmt.Sprintf(worktreeRootTokenFormat, r.worktreeRoots+1)) {
		r.worktreeRoots++
	}
}

// noteAFHome registers the AF home under its own single token. It is not
// numbered: there is exactly one per run.
func (r *redactor) noteAFHome(path string) {
	r.noteRoot(path, afHomeToken)
}

// noteRoot registers one root under an exact token, reporting whether it was
// new. A path already registered keeps its FIRST token — two sessions in one
// repo must read as one repo, and an AF home that is also some session's repo
// must not gain a second name.
func (r *redactor) noteRoot(path, token string) bool {
	path = normalizeRoot(path)
	if path == "" {
		return false
	}
	if _, seen := r.rootTokens[path]; seen {
		return false
	}
	if r.rootTokens == nil {
		r.rootTokens = make(map[string]string)
	}
	r.rootTokens[path] = token
	r.roots = append(r.roots, pathRoot{path: path, token: token})
	return true
}

// normalizeRoot accepts only an absolute directory worth naming, with trailing
// separators removed so the prefix test below has one form to match.
//
// The filesystem root is rejected on purpose: a token standing for "/" would
// rewrite every absolute path in the bundle to "[repo:1]/…", which names
// nothing and destroys every other collapse. It is the same guard scrub already
// applies to a home directory of "/".
func normalizeRoot(path string) string {
	path = strings.TrimRight(path, string(filepath.Separator))
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	return path
}

// rootReplacements returns every path→token rewrite this run knows, LONGEST
// FIRST, with $HOME as the last and least specific of them.
//
// $HOME is part of this list rather than a separate pass because the two are one
// ordered decision: the AF home usually sits INSIDE $HOME, so collapsing $HOME
// first would rewrite "~/.agent-factory" and leave the more specific token
// unreachable. It is the same prefix-shadowing rule sortLongestFirst enforces
// for titles and usernames, and it is part of the privacy invariant for the same
// reason.
//
// Each root is also matched in its DISPLAY spelling, for the reason scrub
// collapses both spellings of $HOME: a path whose bytes are not valid UTF-8
// reaches a bundle through JSON, and through the archive warning's rewritten
// retained root, as replacement characters. The two spellings differ only for an
// invalid-UTF-8 root, so this can only ever redact more.
func (r *redactor) rootReplacements() []pathRoot {
	out := make([]pathRoot, 0, 2*len(r.roots)+2)
	out = append(out, r.roots...)
	if r.home != "" && r.home != "/" {
		out = append(out, pathRoot{path: r.home, token: "~"})
	}
	for _, root := range out[:len(out):len(out)] {
		if display := strings.ToValidUTF8(root.path, "\uFFFD"); display != root.path {
			out = append(out, pathRoot{path: display, token: root.token})
		}
	}
	// STABLE, so two entries that compare equal keep registration order. That is
	// not hypothetical tidiness: a repo whose path IS the home directory
	// registers the same string twice, once as "[repo:1]" and once as "~", and an
	// unstable sort would pick between them differently from run to run — a
	// bundle whose redaction of one path flips between two spellings. Registration
	// order puts the roots first, so the more specific name wins.
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].path) != len(out[j].path) {
			return len(out[i].path) > len(out[j].path)
		}
		return out[i].path < out[j].path
	})
	return out
}

// collapseKnownRoots rewrites every directory this run can name to its token,
// most specific first. It is the text-pass half of the root policy: the field
// half (collapsePathField) rewrites the path FIELDS, and this one catches the
// same roots wherever else they appear — a daemon log line, a diagnostic string,
// the daemon status block, a task's project path.
func (r *redactor) collapseKnownRoots(s string) string {
	for _, root := range r.rootReplacements() {
		s = strings.ReplaceAll(s, root.path, root.token)
	}
	return s
}

// collapsePathField rewrites ONE absolute path field so it carries the layout
// without carrying a directory name.
//
// Three outcomes, in order:
//
//   - Under a registered root: the root becomes its token and everything below
//     it survives, so "<worktree>/.af-source-…" still reads as being inside that
//     worktree.
//   - Under $HOME: collapsed to "~", which is what the text scrub has always
//     done — done here as well so the field is correct even for a caller that
//     never runs the scrub.
//   - Under neither: the MARKER. Such a path is an unknown directory name, and
//     shipping it verbatim is precisely the leak this policy exists to close, so
//     losing its shape is the cheaper half of the trade.
//
// What survives below the root is then title-scrubbed, because af names a
// session's directory after the session TITLE
// ("[af-home]/archived/<hash>/<title>"): the root token alone would still ship
// the one free-text value every other field in the record drops. Only the
// REMAINDER goes through that pass, never the token — a session titled "repo"
// would otherwise rewrite "[repo:1]" itself, since both its neighbours there are
// non-word runes.
func (r *redactor) collapsePathField(path string) string {
	if path == "" {
		return ""
	}
	for _, root := range r.rootReplacements() {
		if rest, ok := underRoot(path, root.path); ok {
			return root.token + r.scrubSessionTitles(rest)
		}
	}
	return redactedMarker
}

// underRoot reports whether path IS root or sits inside it, returning what
// follows the root. The separator is required rather than assumed: without it
// "/srv/repo-backup" would read as a path inside "/srv/repo" and be rewritten to
// a token it has nothing to do with.
func underRoot(path, root string) (string, bool) {
	root = normalizeRoot(root)
	if root == "" {
		return "", false
	}
	if path == root {
		return "", true
	}
	if strings.HasPrefix(path, root+string(filepath.Separator)) {
		return path[len(root):], true
	}
	return "", false
}
