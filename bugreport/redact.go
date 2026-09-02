package bugreport

import (
	"encoding/json"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/internal/credscrub"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// Redaction markers. `redactedMarker` replaces a whole free-text field the
// policy always drops (session titles, session prompts, task prompts, tab commands);
// `secretMarker` replaces a substring a best-effort pattern flagged as a
// credential inside otherwise-kept text (log lines, config values).
const (
	redactedMarker = credscrub.RedactedMarker
	secretMarker   = credscrub.SecretMarker
	userMarker     = "[user]"
)

// afTmuxSessionName matches a repo-scoped af tmux session name
// (af_<8 hex>_<title>, incl. any __tab / _paste suffix). The <title> segment is
// the sanitized, whitespace-stripped session title, so it leaks the same
// free-text name the structured redactor already drops from InstanceData.TmuxName
// — but the daemon log tail is bundled verbatim and prints these names on nearly
// every line (e.g. "af_0f8fc14c_fix-1436"), reintroducing the #1533 leak class
// through the log blob (#1584). The title segment is a run of non-whitespace,
// non-':' characters: titles never contain whitespace (stripped at
// sanitization) and never contain ':' (rewritten to '_'), so ':' — a tmux
// window/pane ref or log delimiter — cleanly bounds the name without ever
// truncating a real title mid-way and leaving a fragment behind. Keys on the
// name *shape*, so it scrubs archived/killed sessions no live set still knows.
var afTmuxSessionName = regexp.MustCompile(`af_[0-9a-f]{8}_[^\s:]+`)

// taskStartedInstanceTitle and taskParkedInstanceTitle recognize the two legacy
// daemon log shapes that rendered a raw session title with %s. New logs use %q,
// but a bundled tail can contain lines written by an older binary. Match the
// legacy field by its fixed surrounding syntax so punctuation-only titles can
// be removed without treating "." or "/" as a global search pattern. The parked
// form keeps its diagnostic suffix; the started form owns the rest of its line.
var (
	taskStartedInstanceTitle = regexp.MustCompile(`(?m)(task \S+ started successfully as instance )[^\r\n]+$`)
	taskParkedInstanceTitle  = regexp.MustCompile(`(?m)(task \S+ parked at a usage limit as instance )(.+)(; waiting for the limit window to reset)$`)
)

// The incomplete-archive warning is the one daemon log line that prints the
// names of files a USER chose. session/git renders it in a single place
// (ArchiveReport.warningSuffix), in a single shape:
//
//	<operation> completed with an incomplete archive: af skipped <N> unreadable
//	files; complete original tree(s) were retained at "<root>"[, "<root>"][, and
//	<N> more in archive_report]; skipped paths[ (showing first <X> of <Y>)]:
//	"<path>" (<reason>)[, "<path>" (<reason>)]
//
// The matchers below key on THAT SHAPE and on nothing else — no live
// ArchiveReport, no note* call, no per-run state — because the log outlives the
// state such a key would come from (#3553): an instances.json that fails
// the typed decode never reaches the typed walk that could record the paths, a
// session killed after the warning was written leaves neither report nor row
// behind, and a bundled tail can hold lines an older binary wrote. Reading the
// renderer instead is also what makes ONE pass over the (up to 2 MiB) tail
// enough, however many unreadable files a generated tree produced.
//
// The renderer being the only source of this shape is the cost of that: a change
// to the format silently stops the scrub. It is paid down in the tests, which
// build every input from the real ArchiveReport.Warning rather than from a
// hand-written fixture, so format and matcher cannot drift apart without a red
// test.
const (
	// archiveWarningQuotedPattern matches one %q-rendered value. strconv.Quote
	// escapes every interior `"` as `\"` and every `\` as `\\`, so this is an
	// exact tokenizer for the renderer's output, not an approximation of one.
	archiveWarningQuotedPattern = `"(?:[^"\\]|\\.)*"`
	archiveWarningRetainedItem  = `(?:` + archiveWarningQuotedPattern + `|and \d+ more in archive_report)`
	archiveWarningSkippedItem   = archiveWarningQuotedPattern + ` \([^()\r\n]*\)`
)

var (
	// archiveWarningRetainedAt finds one warning and captures the rest of its
	// line. It anchors on the renderer's most distinctive literal, which sits
	// immediately before the first path; %q never emits a raw newline, so the
	// whole clause is always on that one line, and collectLog drops the partial
	// first line of a truncated tail, so a bundled clause is never half a line.
	archiveWarningRetainedAt = regexp.MustCompile(`complete original tree\(s\) were retained at ([^\r\n]*)`)
	// archiveWarningTail is the renderer's grammar for that remainder, in four
	// groups: the retained-root list, the label between them, the skipped-path
	// list (empty when a tree was retained with nothing recorded under it), and
	// whatever a caller appended to the same line.
	//
	// That last group is why the invariant holds: it admits no `"` at all, so
	// EVERY quoted token in a parsed remainder sits in group 1 or group 3 and is
	// rewritten. A wrapper that adds quote-free text — `failedArchiveWithHook`
	// appends "(its on-archive hook also failed: …)" to a joined error whose last
	// line is this warning — keeps its diagnostics; one that adds a quoted token
	// fails the grammar and takes the fail-safe path in redactArchiveWarningTail.
	archiveWarningTail = regexp.MustCompile(
		`\A(` + archiveWarningRetainedItem + `(?:, ` + archiveWarningRetainedItem + `)*)` +
			`(; skipped paths(?: \(showing first \d+ of \d+\))?: )` +
			`((?:` + archiveWarningSkippedItem + `(?:, ` + archiveWarningSkippedItem + `)*)?)` +
			`([^"\r\n]*)\z`)
	archiveWarningQuoted = regexp.MustCompile(archiveWarningQuotedPattern)
)

// redactor holds the per-run redaction context — the home directory to
// collapse to "~" and the username token(s) to blank to "[user]" — resolved
// once so every section scrubs against the same values. Constructed with
// newRedactor() in production; tests build one directly with fixed values for
// deterministic assertions.
type redactor struct {
	home  string
	users []string
	// tmuxNames and titles are the known session tmux names and raw session
	// titles gathered while redacting instances and tasks. scrubSessionTitles
	// uses titles for both structured task status strings and the verbatim log;
	// scrubLog additionally removes non-repo-scoped names (af_<title>, no hash,
	// which the afTmuxSessionName shape can't match) — closing the #1584 leak the
	// structured sections don't reach.
	tmuxNames map[string]struct{}
	titles    map[string]struct{}
}

// newRedactor resolves the redaction context from the environment: the OS
// home directory and the current username (plus the home directory's base
// name, which is the username on a conventional layout).
func newRedactor() *redactor {
	home, _ := os.UserHomeDir()
	var users []string
	if u, err := user.Current(); err == nil {
		users = appendUserToken(users, u.Username)
	}
	if home != "" {
		users = appendUserToken(users, filepath.Base(home))
	}
	return &redactor{home: home, users: users}
}

// appendUserToken adds a username token to the scrub list — BOTH the raw form and,
// when they differ, its lowercased form. The username scrub matches byte-exact, but
// the branch stored in instances.json is strings.ToLower(username)
// (config.BranchPrefix), so a mixed-case account ("Sachin.Iyer") would otherwise
// ship its lowercased branch prefix ("sachin.iyer/…") verbatim — the home-to-tilde
// collapse cannot catch it (different case, not a path). Registering both closes
// that leak for the same class as the non-word-boundary one (#2533).
func appendUserToken(users []string, name string) []string {
	name = strings.TrimSpace(name)
	for _, variant := range []string{name, strings.ToLower(name)} {
		users = addUserVariant(users, variant)
		// And the display spelling of each, for the same reason scrub() collapses
		// both spellings of $HOME: a username that is not valid UTF-8 reaches a
		// bundle through JSON, and through the archive warning's rewritten
		// retained root, as replacement characters. addUserVariant dedupes, so on
		// every ordinary account this adds nothing.
		users = addUserVariant(users, strings.ToValidUTF8(variant, "\uFFFD"))
	}
	return users
}

// addUserVariant adds one username spelling to the scrub list, skipping empties,
// path-ish junk, and tokens under 3 chars (too short to replace safely without
// mangling unrelated substrings), and deduping.
func addUserVariant(users []string, name string) []string {
	if len(name) < 3 || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return users
	}
	for _, existing := range users {
		if existing == name {
			return users
		}
	}
	return append(users, name)
}

// scrub is the catch-all text pass applied to every section: it removes PEM
// blocks and pattern-matched credentials, collapses the home directory to "~",
// and blanks bare username tokens to "[user]". It runs last over already
// field-redacted content, so it is defense-in-depth, not the only line of
// defense.
func (r *redactor) scrub(s string) string {
	s = credscrub.Scrub(s)
	if r.home != "" && r.home != "/" {
		s = strings.ReplaceAll(s, r.home, "~")
		// A home directory whose bytes are not valid UTF-8 also reaches a bundle
		// in its DISPLAY spelling: encoding/json cannot carry the raw bytes, and
		// the archive warning's retained root is rewritten to that same form
		// (redactArchiveWarningTail). Matching only the raw token would leave the
		// user's home path — and the username inside it — in both. The two
		// spellings differ only when the home path is invalid UTF-8, and the
		// display form then contains U+FFFD, so this can only ever redact more.
		if display := strings.ToValidUTF8(r.home, "\uFFFD"); display != r.home {
			s = strings.ReplaceAll(s, display, "~")
		}
	}
	// Blank bare username tokens with the SAME manual token boundary the title
	// scrub uses, not a `\b<name>\b` regex: a `\b` after the username never matches
	// when the username ends in a non-word rune (an OS username like "test-"), so
	// "test-/fix-login-bug" in a branch leaked the username unredacted — a silent
	// redaction failure in a bundle meant to be safe to share (#2533).
	//
	// Longest-first, exactly as scrubSessionTitles orders titles and for the same
	// reason: a shorter username can be a prefix of a longer one (a raw "jdoe" vs a
	// home basename "jdoe.admin"), and redacting the prefix first destroys the only
	// exact match for the longer token and strands its suffix. The manual boundary
	// makes that prefix-shadowing easier to hit than `\b` did, so the ordering is
	// part of the privacy invariant, not a nicety. Sort a copy so scrub stays a pure
	// read of r.users.
	names := append([]string(nil), r.users...)
	sortLongestFirst(names)
	for _, name := range names {
		s = replaceBareToken(s, name, userMarker)
	}
	return s
}

// scrubUnstructured is the single sanitizer for a free-text scalar or blob
// before it is embedded in any bug-report rendering. In addition to scrub's
// credential/path policy, it removes every known representation of a session
// title. Keeping this separate from scrub is intentional: scrub also runs over
// already-encoded JSON documents, where treating a short title such as "id" as
// bare text would rewrite structural keys. Call this while the value is still a
// value; all later text/JSON renderings then inherit the safe form.
func (r *redactor) scrubUnstructured(s string) string {
	return r.scrub(r.scrubSessionTitles(s))
}

// scrubLog scrubs the daemon log tail. On top of the standard scrub() pass it
// redacts the free-text <title> in every af_<hash>_<title> tmux session name and
// any bare session title the log prints, so the verbatim log blob can't leak the
// session titles the structured sections already drop (#1584 — the exact #1533
// class, reintroduced through the bundled log). Call this instead of scrub() for
// the log section; it ends by delegating to scrub() for the usual
// $HOME/username/secret pass.
func (r *redactor) scrubLog(s string) string {
	// The incomplete-archive warning goes first, because it is the one pass here
	// that reads the emitter's LITERAL PROSE, and every pass below rewrites text
	// anywhere it appears. A session titled "paths" is enough to break it: the
	// title pass rewrites the renderer's own "skipped paths:" label to
	// "skipped [redacted]:", the anchor is gone before this pass looks for it,
	// and every user file name in the list ships. It is also the ordering the
	// review on #3554 asked for from the other side — a title INSIDE a skipped path
	// ("secret" in "docs/secret-plan.txt") used to be rewritten first and strand
	// the rest of the name — and matching the whole quoted token makes that
	// impossible in either order.
	s = scrubArchiveWarningPaths(s)
	// Remove every known full title representation before any shape-based pass
	// can consume only part of it. In particular, the legacy raw task-start
	// matcher is line-oriented while a legal title may contain newlines; running
	// that matcher first replaced line one and made the original full-title match
	// impossible, leaking the remaining lines (#2249 late review).
	s = r.scrubSessionTitles(s)
	// Redact the title in every af_<hash>_<title> name. Keys on the name shape,
	// so it catches current AND historical (archived/killed) sessions the live
	// instance set no longer references.
	s = afTmuxSessionName.ReplaceAllStringFunc(s, redactAFTmuxTitle)
	// Non-repo-scoped names (af_<title>, no hash) don't match the shape above;
	// redact those known names exactly.
	for name := range r.tmuxNames {
		if !afTmuxSessionName.MatchString(name) {
			s = strings.ReplaceAll(s, name, tmuxPrefixMarker)
		}
	}
	// Retain compatibility with the two legacy raw %s taskrun.go forms. Their
	// syntax is a safer boundary than a global punctuation matcher and also
	// catches historical task-created titles no longer present in instances.json.
	s = taskStartedInstanceTitle.ReplaceAllString(s, `${1}`+redactedMarker)
	s = taskParkedInstanceTitle.ReplaceAllString(s, `${1}`+redactedMarker+`${3}`)
	return r.scrub(s)
}

// scrubSessionTitles removes exact Go-quoted forms of every known title, then
// applies the conservative word-bearing bare-title matcher. The quoted form is
// the important invariant for task targets: daemon delivery logs and persisted
// delivery errors both format them with %q. Matching strconv.Quote therefore
// covers every legal title byte-for-byte, including short names and punctuation
// that are unsafe to replace globally, plus quotes/backslashes that %q escapes
// (#2238 review). scrubLog handles legacy raw punctuation emitters by their
// fixed field syntax.
func (r *redactor) scrubSessionTitles(s string) string {
	titles := make([]string, 0, len(r.titles))
	for title := range r.titles {
		titles = append(titles, title)
	}
	// A shorter title may be a prefix of a longer one. Redacting the prefix
	// first destroys the only exact match for the longer secret and leaves its
	// suffix behind, so the order is part of the privacy invariant. The lexical
	// tie-break makes output deterministic even though titles are stored in a map.
	sortLongestFirst(titles)
	for _, title := range titles {
		s = strings.ReplaceAll(s, strconv.Quote(title), strconv.Quote(redactedMarker))
		s = replaceBareTitle(s, title)
	}
	return s
}

// scrubArchiveWarningPaths takes the user-chosen file names out of every
// incomplete-archive warning in a log tail, and rewrites the retained root
// printed beside them. It is a plain function, not a redactor method, because it
// needs nothing from the run: see the matchers above for why keying on the
// renderer rather than on live session state is the point.
func scrubArchiveWarningPaths(s string) string {
	clauses := archiveWarningRetainedAt.FindAllStringSubmatchIndex(s, -1)
	if clauses == nil {
		return s
	}
	var out strings.Builder
	copied := 0
	for _, clause := range clauses {
		// clause[2]:clause[3] is the captured remainder of the line; everything
		// before it (the anchor prose included) is copied through untouched.
		out.WriteString(s[copied:clause[2]])
		out.WriteString(redactArchiveWarningTail(s[clause[2]:clause[3]]))
		copied = clause[3]
	}
	out.WriteString(s[copied:])
	return out.String()
}

// redactArchiveWarningTail rewrites what one warning prints after "retained
// at ": the retained roots, then the skipped file names with their reasons.
//
// The two lists are treated differently because the policy for the two VALUES
// differs, and this keeps the log agreeing with the JSON section instead of
// inventing a second policy for the same fields:
//
//   - A skipped path is a RELATIVE name a user chose for a file af could not
//     read, so it goes — exactly as redactInstanceData blanks
//     archive_report.retained_trees[].skipped[].path.
//   - A retained root is a SYSTEM path, which that same function keeps on
//     purpose so the closing scrub can collapse $HOME in it. What the log adds
//     is the %q escaping, and that escaping IS the leak: a root that is not
//     valid UTF-8 arrives as `\xNN` escapes, which no home or username
//     replacement can see through. Rewriting the token to the DISPLAY spelling
//     the JSON copy carries drops the raw bytes and hands the collapse something
//     it can match — and for a root that is already valid UTF-8, which is every
//     ordinary one, that rewrite is the identity.
//
// A remainder that does not parse as the renderer's grammar is dropped WHOLE.
// The format moved, or something appended a quoted token of its own; either way
// the quoted runs can no longer be told apart, and a bundle that loses a skip
// reason is a nuisance where one that ships a user's file names is the bug being
// fixed. Between the grammar and that fallback, no quoted token in a matched
// clause can reach the bundle unrewritten.
func redactArchiveWarningTail(tail string) string {
	parsed := archiveWarningTail.FindStringSubmatchIndex(tail)
	if parsed == nil {
		return redactedMarker
	}
	var out strings.Builder
	out.WriteString(archiveWarningQuoted.ReplaceAllStringFunc(
		tail[parsed[2]:parsed[3]], archiveWarningDisplayRoot))
	// The label, kept verbatim: it carries the "(showing first X of Y)" count
	// triage reads. Like the trailing group, it holds no quoted token.
	out.WriteString(tail[parsed[4]:parsed[5]])
	out.WriteString(archiveWarningQuoted.ReplaceAllStringFunc(
		tail[parsed[6]:parsed[7]],
		func(string) string { return strconv.Quote(redactedMarker) }))
	out.WriteString(tail[parsed[8]:parsed[9]])
	return out.String()
}

// archiveWarningDisplayRoot rewrites one %q-rendered retained root to the
// display spelling of the same path — the form encoding/json can carry, and the
// form the $HOME collapse can match. A token that will not unquote did not come
// from this renderer, so it is redacted rather than guessed at.
func archiveWarningDisplayRoot(token string) string {
	raw, err := strconv.Unquote(token)
	if err != nil {
		return strconv.Quote(redactedMarker)
	}
	return strconv.Quote(strings.ToValidUTF8(raw, "\uFFFD"))
}

// sortLongestFirst keeps redaction order in one place: replace longer secrets
// before their prefixes, with a lexical tie-break for deterministic output.
func sortLongestFirst(values []string) {
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
}

// tmuxPrefixMarker is the redaction of an af tmux session name whose title
// segment is removed but whose "af_" prefix is kept so the line still reads as
// referring to an af session.
const tmuxPrefixMarker = "af_" + redactedMarker

// redactAFTmuxTitle redacts the <title> of a matched af_<8 hex>_<title> name,
// keeping the fixed, user-text-free "af_<hash>_" prefix (3 + 8 + 1 = 12 chars).
func redactAFTmuxTitle(match string) string {
	return match[:12] + redactedMarker
}

// replaceBareTitle removes a title only when it occupies a complete text token.
// The legacy logger's raw %s form is delimited by surrounding prose/newlines, so
// this covers that representation without compiling single-line punctuation-only
// titles such as "." or "/" into an unbounded matcher that erases every period
// or path separator in the bundle. A multiline title is different: its exact,
// byte-identical cross-line sequence must be removed before a legacy line matcher
// can consume line one and strand the rest. Exact %q forms are handled above.
//
// A token boundary means start/end of text or a neighboring rune that is not a
// letter, number, combining mark, or underscore. Checking both edges regardless
// of the title's own first/last character handles titles such as "client[prod]"
// while refusing to match "." inside "1.2" or "/" inside "repo/path".
func replaceBareTitle(s, title string) string {
	return replaceBareToken(s, title, redactedMarker)
}

// replaceBareToken replaces token with marker only where token occupies a
// COMPLETE text token — start/end of text or a neighboring rune that is not a
// letter, number, mark, or underscore. It is the manual-boundary replacement both
// the title scrub and the username scrub use, because a `\b<token>\b` regex only
// anchors at word↔non-word transitions: a token that itself ENDS (or starts) in a
// non-word rune — a username like "test-", or a title like "client[prod]" — has no
// `\b` after its trailing "-", so `\b` never matches it and it leaks (#2533). This
// checks the actual neighboring runes instead, so "test-" is redacted in
// "test-/fix-login-bug" (the "/" is a non-word boundary) but not inside a larger
// word.
func replaceBareToken(s, token, marker string) string {
	if strings.TrimSpace(token) == "" || (!containsWordRune(token) && !strings.ContainsAny(token, "\r\n")) {
		return s
	}
	var out strings.Builder
	scan, copied := 0, 0
	changed := false
	for scan <= len(s)-len(token) {
		rel := strings.Index(s[scan:], token)
		if rel < 0 {
			break
		}
		start := scan + rel
		end := start + len(token)
		if titleTokenBoundary(s, start, end) && !insideRedactionMarker(s, start, end) {
			out.WriteString(s[copied:start])
			out.WriteString(marker)
			copied = end
			scan = end
			changed = true
			continue
		}
		// Advance one byte past this rejected occurrence. strings.Index remains
		// byte-based too, so this cannot skip a later exact byte sequence.
		scan = start + 1
	}
	if !changed {
		return s
	}
	out.WriteString(s[copied:])
	return out.String()
}

func titleTokenBoundary(s string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:start])
		if isWordRune(r) {
			return false
		}
	}
	if end < len(s) {
		r, _ := utf8.DecodeRuneInString(s[end:])
		if isWordRune(r) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r)
}

func containsWordRune(s string) bool {
	for _, r := range s {
		if isWordRune(r) {
			return true
		}
	}
	return false
}

// insideRedactionMarker keeps the title sanitizer idempotent when a legal title
// is itself "redacted", "secret", or another substring of a marker emitted by
// an earlier title. Such a match is already inside public replacement text; it
// must not grow the marker or destroy its recognizable shape.
func insideRedactionMarker(s string, start, end int) bool {
	for _, marker := range []string{redactedMarker, secretMarker, userMarker} {
		first := start - len(marker) + 1
		if first < 0 {
			first = 0
		}
		for candidate := first; candidate <= start; candidate++ {
			markerEnd := candidate + len(marker)
			if markerEnd >= end && markerEnd <= len(s) && s[candidate:markerEnd] == marker {
				return true
			}
		}
	}
	return false
}

// noteSession records a session's tmux name(s) and raw title(s) before they are
// redacted, so scrubLog can strip them from the log tail. Called on each record
// while collecting instances, i.e. before collectLog runs.
func (r *redactor) noteSession(d *session.InstanceData) {
	r.noteTmuxName(d.TmuxName)
	r.noteTitle(d.Title)
	r.noteTitle(d.Worktree.SessionName)
	for _, tab := range d.Tabs {
		r.noteTmuxName(tab.TmuxName)
	}
	// PendingTabs is the durability staging area for metadata-only rows. It uses
	// the same TabData shape as Tabs, so its names need the same log treatment.
	for _, tab := range d.PendingTabs {
		r.noteTmuxName(tab.TmuxName)
	}
	// A pending-teardown handle names a tmux session the same way a live tab
	// does, and the retry logs it on every daemon start until the kill is
	// confirmed — so the log tail prints these names most often for exactly the
	// sessions whose teardown is stuck (#2776).
	for _, cleanup := range d.PendingTabCleanup {
		r.noteTmuxName(cleanup.TmuxName)
	}
}

// noteTitle records one raw session title for structured-string and log
// scrubbing, skipping blanks.
func (r *redactor) noteTitle(title string) {
	if strings.TrimSpace(title) == "" {
		return
	}
	if r.titles == nil {
		r.titles = make(map[string]struct{})
	}
	r.titles[title] = struct{}{}
}

// noteTmuxName records one raw tmux session name for scrubLog, skipping blanks.
func (r *redactor) noteTmuxName(name string) {
	if name == "" {
		return
	}
	if r.tmuxNames == nil {
		r.tmuxNames = make(map[string]struct{})
	}
	r.tmuxNames[name] = struct{}{}
}

// titleJSONKeys are the object keys whose string value is a raw session title on
// the generic fallback path, mirroring the fields noteSession reads off a typed
// record (InstanceData.Title and Worktree.SessionName). `tmux_name` is handled
// separately — it is a name derived from the title, not the title itself.
var titleJSONKeys = map[string]bool{"title": true, "session_name": true}

// noteUnknownJSON walks a decoded-but-unparseable instances.json payload and
// records every title/tmux-name it carries, so scrubLog can strip them from the
// log tail exactly as it does for records that decoded typed (#1790). It must
// run BEFORE redactUnknownJSON blanks those same values.
//
// The walk is key-driven and shape-agnostic, so it reaches nested title-bearing
// locations (worktree.session_name, tabs[].tmux_name) without assuming the
// record layout the typed decode already rejected.
func (r *redactor) noteUnknownJSON(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			s, isString := val.(string)
			key := strings.ToLower(k)
			switch {
			case !isString:
				r.noteUnknownJSON(val)
			case titleJSONKeys[key]:
				r.noteTitle(s)
			case key == "tmux_name":
				r.noteTmuxName(s)
			}
		}
	case []any:
		for _, e := range t {
			r.noteUnknownJSON(e)
		}
	}
}

// unparsedInstancesNote is emitted (as a JSON string) when instances.json is
// not even valid JSON, so nothing sensitive is surfaced from a payload we
// cannot reason about at all.
const unparsedInstancesNote = `"[instances.json could not be parsed; contents omitted for safety]"`

// redactInstancesJSON parses one repo's instances.json, applies the structural
// field-redaction policy to every record, re-marshals, and scrubs the result.
// The typed decode is intentional and fail-closed: any field the current
// InstanceData does not know about is dropped rather than passed through, so a
// future secret-bearing field cannot leak before the redactor is taught about
// it.
//
// When the payload does NOT decode as []InstanceData (a corrupt or legacy
// shape — e.g. a field whose type has since changed), the typed field-level
// policy can't apply, so we redact MORE, not less (fail-safe — this bundle is
// shared publicly): a generic key-aware walk blanks every value under a
// known-sensitive key (prompts, commands, tokens, paths, arbitrary metadata)
// before the text scrub runs. If it is not even valid JSON, the contents are
// omitted entirely with a note. The fallback is never raw-with-regex-only —
// under-including beats leaking.
func (r *redactor) redactInstancesJSON(raw json.RawMessage) json.RawMessage {
	var datas []session.InstanceData
	if err := json.Unmarshal(raw, &datas); err == nil {
		for i := range datas {
			r.noteSession(&datas[i])
			redactInstanceData(&datas[i])
		}
		if out, marshalErr := json.MarshalIndent(datas, "", "  "); marshalErr == nil {
			return json.RawMessage(r.scrub(string(out)))
		}
	}

	// Fallback: unknown/corrupt shape. Blank sensitive keys generically, then
	// scrub. Omit entirely if the payload is not valid JSON.
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return json.RawMessage(unparsedInstancesNote)
	}
	// Record the titles this payload carries before blanking them, so scrubLog
	// strips them from the log tail too — the typed path above does this via
	// noteSession, and without it a corrupt instances.json redacted the JSON
	// section while leaving bare titles in the bundled log (#1790).
	r.noteUnknownJSON(generic)
	out, err := json.MarshalIndent(redactUnknownJSON(generic), "", "  ")
	if err != nil {
		return json.RawMessage(unparsedInstancesNote)
	}
	return json.RawMessage(r.scrub(string(out)))
}

// sensitiveJSONKeys are object keys whose values are dropped wholesale on the
// generic fallback path, where the typed field-level policy cannot apply. It is
// deliberately broad and fail-safe: on a shape we could not parse, a key that
// *might* hold free text, a secret, a path, or arbitrary metadata is redacted
// rather than trusted. Structural keys (id, status, program, timestamps, git
// SHAs, counts, flags) are absent here and so survive the walk (then get the
// text scrub for any residual $HOME/username/credential).
var sensitiveJSONKeys = map[string]bool{
	"title": true, "prompt": true, "prompts": true,
	"command": true, "cmd": true, "commands": true,
	"args": true, "argv": true, "arg": true,
	"env": true, "environment": true,
	"token": true, "tokens": true, "secret": true, "secrets": true,
	"password": true, "passwd": true, "pwd": true,
	"credential": true, "credentials": true,
	"api_key": true, "apikey": true, "key": true, "keys": true,
	"auth": true, "authorization": true, "bearer": true,
	"private_key": true, "url": true,
	"path": true, "home": true, "repo_path": true, "worktree_path": true,
	// path_bytes is the durable form of a path that is not valid UTF-8, and JSON
	// carries it BASE64-ENCODED. Blanking "path" alone left the real name in the
	// bundle in a form the closing text scrub cannot recognize as a path, a home
	// directory, or a username — strictly worse than the plain field it stands
	// in for. It is the fallback's copy of the typed clearing in
	// redactInstanceData (#3541).
	"path_bytes":  true,
	"remote_meta": true,
	// A typed record drops this storage-only teardown union in
	// redactInstanceData. Drop the whole object on the generic fallback too: a
	// malformed sibling field must not expose its SSH host/user/key paths, hook
	// command, remote session directory, or container id.
	"runtime_cleanup": true,
	// tmux_name and session_name mirror the typed-path redaction
	// (redactInstanceData drops TmuxName, Worktree.SessionName,
	// Tabs[].TmuxName, and PendingTabCleanup[].TmuxName): each carries the
	// free-text session title. Without them the fallback path leaked titles
	// the typed path already scrubs, including the nested tabs[].tmux_name
	// and worktree.session_name the recursive walk below reaches (#1680).
	// This walk is key-driven and depth-agnostic, which is why it kept
	// covering pending_tab_cleanup[].tmux_name for the whole window the typed
	// path did not (#2776) — the fallback is the broader net by design.
	"tmux_name": true, "session_name": true,
	// conversation and agent_conversation mirror the typed-path redaction
	// (redactInstanceData clears Tabs[].Conversation.ID and
	// AgentConversation.ID): the provider conversation id resumes an agent
	// session and must not ship in a publicly shared bundle. The whole object
	// is dropped rather than just its "id" — on this path the shape is by
	// definition one we could not parse, so a legacy record may carry the id
	// as a bare string, under a differently-named key, or nested deeper, and
	// an id-only rule would miss every such variant. The surviving typed
	// fields (agent, captured_at) are not worth reconstructing a shape
	// contract for here (#1839).
	"conversation": true, "agent_conversation": true,
	// handoffs and from close the same gap on this path that #3405 closed on the
	// typed one: a handoff ledger entry holds the outgoing agent's conversation
	// under the key "from", which neither "conversation" above nor any other key
	// here matched, so an unparseable record leaked exactly the id the typed path
	// had been taught to clear. handoffs drops the ledger wholesale, matching how
	// runtime_cleanup and conversation are handled — on a shape af could not
	// parse, the entry's own layout is not something to rely on. from is the
	// belt: the same object under a legacy or renamed container still goes.
	"handoffs": true, "from": true,
	// pending_handoff_mission mirrors the typed-path redaction (redactInstanceData
	// blanks PendingHandoffMission): the rendered takeover brief embeds the user's
	// free-text prompt/goal verbatim, the same sensitivity class as prompt. A
	// record that fails the typed decode must still drop it (#2419).
	"pending_handoff_mission": true,
}

// redactUnknownJSON recursively rebuilds a decoded JSON value, blanking any
// value whose object key is in sensitiveJSONKeys and recursing everywhere else.
// Non-container leaves are returned unchanged (the caller text-scrubs them).
func redactUnknownJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if sensitiveJSONKeys[strings.ToLower(k)] {
				out[k] = redactedMarker
				continue
			}
			out[k] = redactUnknownJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactUnknownJSON(e)
		}
		return out
	default:
		return v
	}
}

// redactInstanceData blanks the free-text and arbitrary-payload fields of a
// single session record while leaving the structural triage fields (ids,
// liveness/status, program, timestamps, git SHAs, counts, flags) intact.
// Paths are left for the text scrub to collapse ($HOME→~, username→[user]).
func redactInstanceData(d *session.InstanceData) {
	// A kill tombstone's storage-only cleanup handle can contain a private SSH
	// host/user/key path, a hook command path, or a container id. None is needed
	// to diagnose the session shape, and unlike ordinary snapshots instances.json
	// carries it specifically so teardown can resume after restart.
	d.RuntimeCleanup = nil
	if d.Title != "" {
		d.Title = redactedMarker
	}
	if d.Prompt != "" {
		d.Prompt = redactedMarker
	}
	// PendingHandoffMission is a rendered takeover brief that embeds the user's
	// free-text prompt/goal verbatim — the same sensitivity class as Prompt. It
	// was added with transactional handoff (#2286) after this policy was written,
	// so it passed through unredacted into publicly shared bundles (#2419).
	if d.PendingHandoffMission != "" {
		d.PendingHandoffMission = redactedMarker
	}
	if d.Worktree.SessionName != "" {
		d.Worktree.SessionName = redactedMarker
	}
	// TmuxName is derived from the session title (e.g. "af_ConfidentialDeal"),
	// so it leaks the same free-text name Title carries and must be redacted too.
	if d.TmuxName != "" {
		d.TmuxName = redactedMarker
	}
	// A pending-teardown handle is a tmux name and nothing else, so it carries
	// the title exactly as TmuxName above does. It was added with durable tab
	// close (#2669) after this policy was written and inherited none of it, which
	// left the title in pending_tab_cleanup[].tmux_name of every bundle whose
	// instances.json decoded typed — the common case (#2776). The tab id beside
	// it is minted, never derived from user text, and survives for triage.
	for i := range d.PendingTabCleanup {
		if d.PendingTabCleanup[i].TmuxName != "" {
			d.PendingTabCleanup[i].TmuxName = redactedMarker
		}
	}
	for i := range d.Tabs {
		redactTabData(&d.Tabs[i])
	}
	// PendingTabs contains the same TabData shape under a separate ordering and
	// durability contract. Keeping one field redaction function prevents a row
	// from becoming less private merely because restore staged it (#3062).
	for i := range d.PendingTabs {
		redactTabData(&d.PendingTabs[i])
	}
	if d.AgentConversation != nil {
		d.AgentConversation.ID = ""
	}
	if d.PRInfo.Title != "" {
		d.PRInfo.Title = redactedMarker
	}
	if d.PRInfo.URL != "" {
		d.PRInfo.URL = redactedMarker
	}
	// ArchiveReport.Skipped[].Path carries the relative file names a user chose
	// for files af could not read (hence permission_denied). They are relative to
	// the retained worktree root, so the text scrub cannot reach them — there is
	// no $HOME to collapse to "~" and no username to blank to "[user]", and the
	// policy above only covers paths the scrub can collapse. The report arrived
	// with lossless archive storage (#3171) after this policy was written, so a
	// name a user marked private (e.g. "credential", "private-work.txt",
	// "generated/private-019") shipped verbatim in every publicly shared bundle's
	// archive_report.retained_trees[].skipped[].path — the same class as the
	// title leaks in #2419 and #2776, a field added after the policy was written.
	// PathBytes is the durable form when Path is not valid UTF-8 and must be
	// cleared alongside it, or the raw name survives the display redaction. The
	// retained tree's own path and the skip reason survive: the tree path is the
	// system worktree path the scrub collapses via $HOME, and the reason is the
	// diagnostic ("permission denied on N files").
	if d.ArchiveReport != nil {
		for i := range d.ArchiveReport.RetainedTrees {
			// The tree's display Path is kept on purpose (see above), but its
			// PathBytes is not the same field twice: json emits it base64-encoded,
			// and the $HOME/username scrub the display form relies on cannot see
			// through base64. A root whose own name is not valid UTF-8 therefore
			// shipped raw.
			//
			// Clearing PathBytes is not enough on its own: ArchiveRetainedTree's
			// MarshalJSON RE-DERIVES it from Path whenever it is empty, so a Path
			// carrying invalid UTF-8 would put the raw bytes straight back on the
			// wire. Reducing Path to its display form first is what makes the
			// clearing hold, and it makes that an invariant of this function rather
			// than an inherited property of whoever decoded the record. It is
			// lossless for triage: the display form is what the JSON section would
			// have shown anyway, and the text scrub still collapses $HOME in it.
			d.ArchiveReport.RetainedTrees[i].Path = strings.ToValidUTF8(d.ArchiveReport.RetainedTrees[i].Path, "\uFFFD")
			d.ArchiveReport.RetainedTrees[i].PathBytes = nil
			for j := range d.ArchiveReport.RetainedTrees[i].Skipped {
				d.ArchiveReport.RetainedTrees[i].Skipped[j].Path = redactedMarker
				d.ArchiveReport.RetainedTrees[i].Skipped[j].PathBytes = nil
			}
		}
	}
}

func redactTabData(tab *session.TabData) {
	if tab.Command != "" {
		tab.Command = redactedMarker
	}
	if tab.TmuxName != "" {
		tab.TmuxName = redactedMarker
	}
	// A web tab's URL is user-supplied (any http/https target passes
	// NormalizeWebTabURL) and can name internal infrastructure or a private
	// repo — the same class of sensitive URL PRInfo.URL is redacted for
	// below (#1954). External targets are dropped wholesale. For a loopback
	// dev-server, retain only the origin needed for triage: userinfo, paths,
	// queries and fragments can all carry credentials even though the host is
	// safe to name.
	if tab.URL != "" {
		if !session.IsLoopbackWebTarget(tab.URL) {
			tab.URL = redactedMarker
		} else if parsed, err := url.Parse(tab.URL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			tab.URL = redactedMarker
		} else {
			tab.URL = (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
		}
	}
	if tab.Conversation != nil {
		tab.Conversation.ID = ""
	}
	// Handoffs[].From is the same AgentConversationData under a different name,
	// carrying an id with the same resume semantics — so it is the same policy,
	// not an adjacent one (#3405). The ledger arrived with agent handoff (#2013)
	// five days after the conversation-id policy was written and inherited none
	// of it, so every bundle from a session that had ever swapped agents shipped
	// the OUTGOING agent's resumable id while the incoming one was redacted
	// beside it.
	//
	// A loop over the whole ledger rather than a first/last entry: it is
	// append-only and unbounded, and "the ones we thought of" is not a policy.
	for i := range tab.Handoffs {
		tab.Handoffs[i].From.ID = ""
	}
}

// redactedTask is the structural, secret-free projection of a task.Task. The
// prompt and watch command — both free-text that can carry secrets — collapse
// to a marker (and a boolean recording that one was present). LastRunStatus is
// kept for diagnostics after known session titles are removed. ProjectPath
// survives here and is scrubbed for $HOME/username by the text pass.
type redactedTask struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	HasPrompt     bool   `json:"has_prompt"`
	Prompt        string `json:"prompt,omitempty"`
	CronExpr      string `json:"cron_expr,omitempty"`
	HasWatchCmd   bool   `json:"has_watch_cmd"`
	WatchCmd      string `json:"watch_cmd,omitempty"`
	TargetSession string `json:"target_session,omitempty"`
	ProjectPath   string `json:"project_path,omitempty"`
	Program       string `json:"program,omitempty"`
	Enabled       bool   `json:"enabled"`
	LastRunStatus string `json:"last_run_status,omitempty"`
}

// redactTask maps a task.Task to its redacted projection. Recording the target
// here keeps both title defenses inseparable: the structured task field is
// dropped below, and scrubLog removes the same title from daemon log lines.
func (r *redactor) redactTask(t task.Task) redactedTask {
	r.noteTitle(t.TargetSession)
	rt := redactedTask{
		ID:            t.ID,
		Name:          t.Name,
		CronExpr:      t.CronExpr,
		ProjectPath:   t.ProjectPath,
		Program:       t.Program,
		Enabled:       t.Enabled,
		LastRunStatus: r.scrubUnstructured(t.LastRunStatus),
	}
	if t.TargetSession != "" {
		rt.TargetSession = redactedMarker
	}
	if strings.TrimSpace(t.Prompt) != "" {
		rt.HasPrompt = true
		rt.Prompt = redactedMarker
	}
	if strings.TrimSpace(t.WatchCmd) != "" {
		rt.HasWatchCmd = true
		rt.WatchCmd = redactedMarker
	}
	return rt
}

// redactTasks first registers every current task target, then creates the
// secret-free projections. The two passes make task order irrelevant: one
// task's historical status may mention another task's current target.
func (r *redactor) redactTasks(tasks []task.Task) []redactedTask {
	for _, t := range tasks {
		r.noteTitle(t.TargetSession)
	}
	out := make([]redactedTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, r.redactTask(t))
	}
	return out
}
