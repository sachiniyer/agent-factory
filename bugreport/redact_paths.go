package bugreport

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
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

type worktreePathTitle struct {
	repoPath string
	segment  string
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
	r.afHome = ""
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) {
		r.afHome = cleaned
	}
	r.noteRoot(path, afHomeToken)
}

func (r *redactor) noteWorktreeTitle(repoPath, title string) {
	repoPath = filepath.Clean(repoPath)
	if !filepath.IsAbs(repoPath) {
		return
	}
	segment := sessiongit.DerivedWorktreePathTitleSegment(repoPath, title)
	if segment == "" {
		return
	}
	if r.worktreePathTitles == nil {
		r.worktreePathTitles = make(map[worktreePathTitle]struct{})
	}
	r.worktreePathTitles[worktreePathTitle{repoPath: repoPath, segment: segment}] = struct{}{}
}

func (r *redactor) noteWorktreeSubdirectoryTitle(title string) {
	segment := sessiongit.DerivedWorktreeSubdirectoryTitleSegment(title)
	if segment == "" {
		return
	}
	if r.worktreeSubdirectoryTitles == nil {
		r.worktreeSubdirectoryTitles = make(map[string]struct{})
	}
	r.worktreeSubdirectoryTitles[segment] = struct{}{}
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

// collapseKnownRoots rewrites every complete directory this run can name to its
// token, most specific first. It is the text-pass half of the root policy: the
// field half (collapsePathField) rewrites the path FIELDS, and this one catches
// the same roots wherever else they appear — a daemon log line, a diagnostic
// string, the daemon status block, a task's project path. Text boundaries are
// explicit: a registered "/srv/repo" must not consume the prefix of its unrelated
// sibling "/srv/repo-backup" and strand that sibling's private suffix (#4099).
func (r *redactor) collapseKnownRoots(s string) string {
	for _, root := range r.rootReplacements() {
		s = replaceKnownRoot(s, root.path, root.token)
	}
	return s
}

// scrubWorktreePathTitles removes the title-derived segment only in the exact
// sibling path context that gives it meaning. A derived spelling is not swept
// globally: an equal bounded diagnostic or branch value remains intact. Any
// numeric collision suffix is AF-authored and survives as triage information.
func (r *redactor) scrubWorktreePathTitles(s string) string {
	titles := make([]worktreePathTitle, 0, len(r.worktreePathTitles))
	for title := range r.worktreePathTitles {
		titles = append(titles, title)
	}
	sort.Slice(titles, func(i, j int) bool {
		iLen := len(titles[i].repoPath) + len(titles[i].segment)
		jLen := len(titles[j].repoPath) + len(titles[j].segment)
		if iLen != jLen {
			return iLen > jLen
		}
		if titles[i].repoPath != titles[j].repoPath {
			return titles[i].repoPath < titles[j].repoPath
		}
		return titles[i].segment < titles[j].segment
	})
	for _, title := range titles {
		needle := title.repoPath + "-" + title.segment
		replacement := title.repoPath + "-" + redactedMarker
		s = replaceSiblingWorktreeTitle(s, needle, replacement)
	}
	return s
}

func replaceSiblingWorktreeTitle(s, needle, replacement string) string {
	var out strings.Builder
	scan, copied := 0, 0
	changed := false
	for scan <= len(s)-len(needle) {
		rel := strings.Index(s[scan:], needle)
		if rel < 0 {
			break
		}
		start := scan + rel
		end := start + len(needle)
		if derivedWorktreePathBoundary(s, start, end) {
			out.WriteString(s[copied:start])
			out.WriteString(replacement)
			copied = end
			scan = end
			changed = true
			continue
		}
		scan = start + 1
	}
	if !changed {
		return s
	}
	out.WriteString(s[copied:])
	return out.String()
}

func derivedWorktreePathBoundary(s string, start, end int) bool {
	if !pathStartsAt(s, start) {
		return false
	}
	if pathEndsAt(s, end) {
		return true
	}
	if end >= len(s) || s[end] != '-' {
		return false
	}
	// firstFreeWorktreePath appends the lowest available "-N" suffix. Keep that
	// bounded system value, but require all digits and a real path/text boundary
	// so a shorter title cannot consume the prefix of a longer sibling title.
	end++
	digits := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	return end > digits && pathEndsAt(s, end)
}

func pathEndsAt(s string, end int) bool {
	if end == len(s) || s[end] == byte(filepath.Separator) {
		return true
	}
	after, _ := utf8.DecodeRuneInString(s[end:])
	return isPathTextDelimiter(after)
}

func replaceKnownRoot(s, root, token string) string {
	var out strings.Builder
	scan, copied := 0, 0
	changed := false
	for scan <= len(s)-len(root) {
		rel := strings.Index(s[scan:], root)
		if rel < 0 {
			break
		}
		start := scan + rel
		end := start + len(root)
		if knownRootTextBoundary(s, start, end) {
			out.WriteString(s[copied:start])
			out.WriteString(token)
			copied = end
			scan = end
			changed = true
			continue
		}
		scan = start + 1
	}
	if !changed {
		return s
	}
	out.WriteString(s[copied:])
	return out.String()
}

func knownRootTextBoundary(s string, start, end int) bool {
	if !pathStartsAt(s, start) {
		return false
	}
	if end == len(s) || s[end] == byte(filepath.Separator) {
		return true
	}
	// This is the one safe non-separator sibling: an earlier structured or
	// contextual pass has already proven the suffix to be user-title data and
	// replaced it, so collapse the registered repo prefix while retaining both
	// the sibling dash and the marker (and any collision suffix).
	if strings.HasPrefix(s[end:], "-"+redactedMarker) {
		return true
	}
	after, _ := utf8.DecodeRuneInString(s[end:])
	return isPathTextDelimiter(after)
}

func pathStartsAt(s string, start int) bool {
	if start == 0 {
		return true
	}
	before, _ := utf8.DecodeLastRuneInString(s[:start])
	if isPathTextDelimiter(before) {
		return true
	}
	// A URI wrapper can put its first filesystem-path slash immediately after
	// the scheme/authority, so the byte before an absolute root is not a text
	// delimiter (`file:///srv/repo`, `vscode://file/srv/repo`). Accept only that
	// FIRST URI path component: a slash already present after "://" means this
	// root is merely a suffix of a longer URI path and must not be rewritten.
	prefix := s[:start]
	scheme := strings.LastIndex(prefix, "://")
	return scheme >= 0 && !strings.Contains(prefix[scheme+3:], "/")
}

// isPathTextDelimiter names punctuation and whitespace used by renderers around
// a complete path. Shell control operators and backtick wrappers terminate paths
// in command-bearing config values. Letters, numbers and filename punctuation
// such as '.', '_' and '-' deliberately do not qualify: they can continue a
// sibling's basename, which is the distinction collapsePathField's separator
// check makes for an isolated path value.
func isPathTextDelimiter(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("\"'=,:;()[]{}<>&|`", r)
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
