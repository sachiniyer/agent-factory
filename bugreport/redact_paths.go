package bugreport

import (
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
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

type pathBoundary func(string, int, int) bool
type pathStartBoundary func(string, int) bool

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
	r.afHomeSpellings = rootSpellings(path)
	if len(r.afHomeSpellings) > 0 {
		r.afHome = r.afHomeSpellings[0]
	}
	r.noteRoot(path, afHomeToken)
}

// noteWorktreeTitle registers a typed (accepted) record's sibling worktree
// path-title pair. It is the noteSession entry point and is deliberately
// UNCAPPED: a valid instances.json already registers every accepted record,
// and the typed repo_path is a registered root, so the title-derived segment is
// the only thing the sibling redaction must reach. Capping it here would let a
// daemon-log path collapse the repo to a token but ship the past-the-cap title
// segment verbatim beside it (e.g. "[repo:n]-Private-title"). The CPU bound the
// #4938 review asked for belongs to the fallback entry point
// (noteFallbackWorktreeTitle), which a single malformed field can turn into
// thousands of pairs when it falls a whole archive back from the typed decode;
// the typed path has no such explosion, so it keeps redacting every record.
func (r *redactor) noteWorktreeTitle(repoPath, title string) {
	for _, spelling := range absolutePathSpellings(repoPath) {
		segment := sessiongit.DerivedWorktreePathTitleSegment(spelling, title)
		if segment == "" {
			continue
		}
		if r.worktreePathTitles == nil {
			r.worktreePathTitles = make(map[worktreePathTitle]struct{})
		}
		r.worktreePathTitles[worktreePathTitle{repoPath: spelling, segment: segment}] = struct{}{}
	}
}

// noteFallbackWorktreeTitle registers a fallback (rejected-record) sibling
// worktree path-title pair, capped at maxWorktreePathTitles. The bound is on
// this entry point and not on noteWorktreeTitle because the typed path
// (noteSession) redacts every accepted record's title and must not lose a
// segment past the cap, while the fallback only runs when instances.json
// rejects the typed decode — and a single bad field can fall the whole archive
// back, registering every record's title here. Past the cap registration is a
// no-op so the per-pair daemon-log-tail scan in appendWorktreePathTitleSpans
// and the sibling-prefix loop in appendLogOnlyPathBlankSpans each iterate a
// fixed budget rather than the rejected-record count. The cap is fail-closed
// rather than fail-open: once it is saturated, appendLogOnlyPathBlankSpans
// switches the sibling-shape redaction to a single-pass blank of every
// path token, so a past-the-cap sibling spelling such as
// "<repo_path>-<private-title>" does not survive the daemon log verbatim —
// the bare-path blank cannot reach it (the registered repo_path is
// immediately followed by '-', and knownRootTextBoundary accepts that dash
// only once the title pass has already replaced the suffix with -[redacted],
// which cannot happen for a pair the cap dropped). The redacted instances.json
// still drops every title field wholesale, so the cap narrows only the
// log-tail scan (#4938 review).
//
// The cap counts only the fallback pairs added (worktreePathTitlesFallback).
// The shared worktreePathTitles map also holds uncapped typed-record pairs
// (noteWorktreeTitle, called by noteSession); counting those typed pairs
// toward the fallback cap meant a valid large archive that already filled
// the map with typed titles saturated on a later repository's first
// rejected record — one new fallback pair flipped the sibling scrubber to
// blanking every path even though the fallback registry itself never
// approached the fallback cap. The cap belongs to the fallback entry
// point, so the bound is on the fallback-only counter rather than on the
// shared map size (#4938 review).
//
// Saturation is recorded only when a call would actually add a new pair:
// rejected records commonly repeat the same (repo_path, title), and a
// duplicate that adds no matcher must not flip the scrubber to blanking
// every path (#4938 review).
func (r *redactor) noteFallbackWorktreeTitle(repoPath, title string) {
	if r.worktreePathTitles == nil {
		r.worktreePathTitles = make(map[worktreePathTitle]struct{})
	}
	// Collect only the pairs this call would add that are not already
	// registered, so a duplicate (repo_path, title) at the cap is a no-op
	// instead of flipping the sibling scrubber to blanking every path. The
	// shared worktreePathTitles map also holds uncapped typed-record pairs
	// (noteWorktreeTitle, called by noteSession), so a fallback call for a
	// pair the typed path already registered must not saturate either
	// (#4938 review).
	var newPairs []worktreePathTitle
	for _, spelling := range absolutePathSpellings(repoPath) {
		segment := sessiongit.DerivedWorktreePathTitleSegment(spelling, title)
		if segment == "" {
			continue
		}
		pair := worktreePathTitle{repoPath: spelling, segment: segment}
		if _, ok := r.worktreePathTitles[pair]; ok {
			continue
		}
		newPairs = append(newPairs, pair)
	}
	if len(newPairs) == 0 {
		// Every pair was already registered: this duplicate adds no matcher,
		// so do not touch the saturation flag.
		return
	}
	if r.worktreePathTitlesFallback >= maxWorktreePathTitles {
		// Cap reached: stop registering further distinct fallback pairs so
		// appendWorktreePathTitleSpans and the sibling-prefix loop scan a
		// fixed budget rather than one pair per rejected record. The cap
		// counts only the fallback pairs added (worktreePathTitlesFallback),
		// not the typed pairs that share the map, so a valid large archive
		// that already filled the map with typed titles does not saturate
		// here. Dropping the new fallback pair silently would be fail-open
		// — a daemon-log sibling spelling for the omitted record would ship
		// the verbatim repo path and private title segment, and the fallback
		// JSON redaction protects a separate section — so record
		// saturation; the scan then switches to a single-pass fail-closed
		// blank of every path token (#4938 review).
		r.worktreePathTitlesSaturated = true
		return
	}
	for _, pair := range newPairs {
		r.worktreePathTitles[pair] = struct{}{}
		r.worktreePathTitlesFallback++
	}
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

// maxLogOnlyPathBlanks caps the number of distinct verbatim path spellings the
// generic fallback registers for log-scope blanking. The fallback only runs
// when instances.json is valid JSON but rejects the typed []InstanceData decode
// (a hand-edited or legacy shape), and a single repo's instances.json
// realistically carries at most a few hundred distinct repo/worktree/alternate
// spellings — each logical path admits at most a handful of spellings (cleaned,
// resolved, raw), and the map dedupes, so this is a bound on distinct spellings
// rather than on records.
//
// The cap bounds the O(paths × tail) scan in appendLogOnlyPathBlankSpans: one
// malformed field in an otherwise-large archive used to fall the whole payload
// back from the typed decode and register every record's distinct
// worktree_path, so a ten-thousand-record archive plus one type mismatch made
// af bug-report scan the 2 MiB daemon-log tail once per registered path while
// handling already-corrupted state. Beyond the cap the registration is a no-op,
// so the scan iterates over a fixed budget instead of over the record count
// (#4938 review).
//
// This narrows only the LOG-tail verbatim blanking. The redacted instances.json
// itself still blanks every path field wholesale via sensitiveJSONKeys
// (redactUnknownJSON), so a path past the cap can survive the daemon log section
// but the private directory still does not ship in the redacted JSON; the
// capability the typed path already collapses to a numbered root is unaffected
// for everything under the cap. The value covers any realistic single-repo
// instance set with large margin and bounds the pathological CPU.
const maxLogOnlyPathBlanks = 4096

// maxWorktreePathTitles caps the distinct (repo_path, title-segment) pairs the
// FALLBACK entry point (noteFallbackWorktreeTitle) registers for the sibling
// worktree-path title redaction. appendWorktreePathTitleSpans and the
// sibling-prefix loop in appendLogOnlyPathBlankSpans each scan the (up to 2 MiB)
// daemon-log tail once per registered pair, so a single repo_path with
// thousands of distinct rejected titles made those scans unbounded even with
// the path-blank cap in place. The map dedupes, so this is a bound on distinct
// pairs rather than on records; realistic single-repo instance sets stay far
// below it. The cap is on the fallback only: the typed entry point
// (noteWorktreeTitle, called by noteSession) is uncapped so a valid large
// archive keeps redacting every sibling title segment rather than letting a
// daemon-log path such as "[repo:n]-Private-title" ship the segment verbatim
// once it crosses the cap (#4938 review).
const maxWorktreePathTitles = 4096

// noteLogOnlyPathRedaction registers an absolute repo path gathered from the
// generic fallback (noteUnknownJSONRecord) for log-scope blanking only. The
// typed path registers the same value as a named root via noteRepoRoot so it
// collapses to a numbered token everywhere; #4115 deliberately declines to
// trust an untyped repo_path that way. This is the fallback's substitute: the
// verbatim string is blanked to the marker in the daemon log tail (and any
// diagnostic provenance that feeds the same matchers) without ever entering
// r.roots or r.rootTokens, so the value has no structural role in any other
// section. It mirrors the spellings noteWorktreeTitle stores for the same
// value, so the sibling-prefix blank and the worktree-title-segment
// redaction recognize the same occurrence.
//
// Registration is capped at maxLogOnlyPathBlanks distinct spellings; once the
// cap is reached further paths are a no-op, bounding the log-tail scan to a
// fixed budget instead of to the rejected-record count. The cap is fail-closed
// rather than fail-open: once it is saturated, appendLogOnlyPathBlankSpans
// switches from the per-needle scan to a single-pass blank of every absolute
// path, so a record registered past the cap does not survive the daemon log
// verbatim either. Saturation is recorded only when a call would actually add
// a new spelling: rejected records commonly repeat the same repo_path, and a
// duplicate that adds no needle must not flip the scrubber to blanking every
// absolute path (#4938 review).
func (r *redactor) noteLogOnlyPathRedaction(path string) {
	if r.logOnlyPathBlanks == nil {
		r.logOnlyPathBlanks = make(map[string]struct{})
	}
	if r.logOnlyPathBareNames == nil {
		r.logOnlyPathBareNames = make(map[string]struct{})
	}
	// Collect this call's slash-bearing spellings (cleaned + raw absolute,
	// and multi-segment relative) and keep only the ones not already
	// registered, so a duplicate is a no-op even at the cap instead of
	// switching the log scrubber to a blank-every-path pass (#4938 review).
	var newSlashSpellings []string
	for _, spelling := range absolutePathSpellings(path) {
		if _, ok := r.logOnlyPathBlanks[spelling]; !ok {
			newSlashSpellings = append(newSlashSpellings, spelling)
		}
	}
	if raw := originalAbsolutePathSpelling(path); raw != "" {
		// Persisted worktrees keep the raw path string verbatim (double
		// separators, trailing slashes, un-resolved "."/".." segments and the
		// like) and DiagnoseMissingWorktree logs those raw strings.
		// absolutePathSpellings cleans its input, so a raw spelling like
		// "/srv//ConfidentialClient/repo" would otherwise register only
		// "/srv/ConfidentialClient/repo" and the log-only matcher could not
		// find the verbatim value in the daemon log. Keep the original
		// absolute spelling too, matched only as a log-only blank.
		if _, ok := r.logOnlyPathBlanks[raw]; !ok {
			newSlashSpellings = append(newSlashSpellings, raw)
		}
	}
	// NewGitWorktreeFromStorage only rejects empty paths, so a rejected
	// record can carry a relative repo_path/worktree_path (e.g.
	// "private-client/repo"); absolutePathSpellings returns nil for it,
	// and logVanishedWorktreeOnce then writes the stored value with %q,
	// shipping the verbatim relative path in a tail line such as
	// repo_path="private-client/repo" even though the fallback JSON masks
	// that field in a separate section. Register the slash-bearing
	// relative spelling for log-scope blanking too so the verbatim value
	// does not survive the daemon log tail (#4938 review).
	//
	// Single-segment relative spellings (no path separator after cleaning)
	// are routed to a separate set (logOnlyPathBareNames) — the saturated
	// scan anchors on '/', so it cannot reach them once the slash-bearing
	// path cap saturates. The bare-name set's own per-needle scan reaches
	// them regardless of saturation; see relativeBareNameSpellings for the
	// over-blank trade-off the #4938 review accepted (#4938 review).
	var newBareNameSpellings []string
	for _, spelling := range relativePathSpellings(path) {
		if strings.ContainsRune(spelling, filepath.Separator) {
			if _, ok := r.logOnlyPathBlanks[spelling]; !ok {
				newSlashSpellings = append(newSlashSpellings, spelling)
			}
			continue
		}
		if _, ok := r.logOnlyPathBareNames[spelling]; !ok {
			newBareNameSpellings = append(newBareNameSpellings, spelling)
		}
	}
	if len(newSlashSpellings) == 0 && len(newBareNameSpellings) == 0 {
		// Every spelling was already registered: this duplicate adds no
		// needle, so do not touch the saturation flag.
		return
	}
	if len(newSlashSpellings) > 0 && len(r.logOnlyPathBlanks) >= maxLogOnlyPathBlanks {
		// Slash-bearing cap reached: stop registering further distinct
		// slash-bearing spellings so appendLogOnlyPathBlankSpans scans a
		// fixed budget rather than one needle per rejected record. Dropping
		// them silently would be fail-open — a daemon-log tail line for an
		// omitted record would ship its private path verbatim, and the
		// fallback JSON redaction protects a separate section — so record
		// saturation; the scan then switches to a single-pass fail-closed
		// blank of every absolute path (#4938 review).
		r.logOnlyPathBlanksSaturated = true
	} else {
		for _, spelling := range newSlashSpellings {
			r.logOnlyPathBlanks[spelling] = struct{}{}
		}
	}
	// Bare names are bounded by a separate cap on logOnlyPathBareNames.
	// Saturating the bare-name set (more than maxLogOnlyPathBlanks distinct
	// single-segment relative spellings, an implausible count for any
	// realistic rejected-record stream) used to drop further spellings
	// silently: the saturated scan anchors on '/' and cannot reach a bare
	// name, and the per-needle pass has no entry for a dropped name, so a
	// daemon-tail line for an omitted record such as
	// repo_path="ConfidentialClient4097" shipped the private name verbatim.
	// Record saturation (logOnlyPathBareNamesSaturated); the matcher then
	// fail-closed-blanks every decoded single %q scalar that carries no '/'
	// in its entirety, so the past-the-cap bare name does not survive the
	// daemon log (#4938 review).
	//
	// Saturation is recorded only when a call would actually add a new
	// spelling: newBareNameSpellings already excludes names the set holds,
	// so a duplicate bare name (the common rejected-record repeat) adds no
	// needle and must not flip the scrubber to the fail-closed blank, the
	// same guard the slash-bearing cap applies (#4938 review).
	if len(newBareNameSpellings) > 0 && len(r.logOnlyPathBareNames) >= maxLogOnlyPathBlanks {
		r.logOnlyPathBareNamesSaturated = true
	} else {
		for _, spelling := range newBareNameSpellings {
			r.logOnlyPathBareNames[spelling] = struct{}{}
		}
	}
}

// originalAbsolutePathSpelling returns the path as AF received it, before
// absolutePathSpellings cleans it, when the raw spelling is itself absolute and
// cleaning would change it. It is not a root candidate — it is not normalized,
// so it has no structural role and never enters r.roots or r.rootTokens — and is
// matched only as a verbatim log-only blank, so a daemon log that carried the
// raw double-slash or trailing-slash spelling is blanked too.
func originalAbsolutePathSpelling(path string) string {
	if !filepath.IsAbs(path) {
		return ""
	}
	if path == filepath.Clean(path) {
		return ""
	}
	return path
}

// relativePathSpellings returns the log-scope-blank spellings of a RELATIVE
// path, complementing absolutePathSpellings (which returns nil for relative
// paths) and originalAbsolutePathSpelling (which is absolute-only). A rejected
// record can carry a relative repo_path or worktree_path —
// NewGitWorktreeFromStorage only rejects empty paths, and a hand-edited or
// legacy instances.json can hold anything — and logVanishedWorktreeOnce then
// writes that stored value with %q, so a tail line such as
// repo_path="private-client/repo" ships the verbatim relative path even though
// the fallback JSON redacts that field in a separate section. Registering the
// cleaned spelling lets the log-tail blank reach it the same way an absolute
// path's spelling is reached (#4938 review).
//
// The spellings are not root candidates — they are not absolute, so they have
// no structural role and never enter r.roots or r.rootTokens — and are matched
// only as verbatim log-only blanks. A raw spelling that differs from the
// cleaned one (e.g. "./private-client/repo", "a/./b", "a//b") is kept alongside
// the cleaned form, the same way originalAbsolutePathSpelling keeps the raw
// absolute spelling: a daemon log can carry the raw bytes and the cleaned
// needle would miss them.
//
// Single-segment relative names (no path separator after cleaning) ARE
// registered, on the privacy side of the #4938 review's trade-off. The
// saturated scan anchors on '/' and cannot reach them once the slash-bearing
// path cap saturates, so noteLogOnlyPathRedaction routes them to a separate
// set (logOnlyPathBareNames) whose bounded per-needle pass always runs; and a
// daemon log line that names a private directory of the codebase verbatim
// (e.g. parent_path="ConfidentialClient" derived from
// worktree_path="ConfidentialClient/wt", or a single-segment repo_path of the
// same shape) is no more "prose" than the path itself, so blanking the bare
// directory name is the same privacy contract the typed and slash-bearing
// fallback already make. Trivially-empty cleaned spellings (".", "..") are
// skipped — they name no private directory — but the RAW spelling that
// filepath.Clean collapses to one (e.g. "ConfidentialClient/..") is kept,
// because the daemon log emits that raw value via %q and the cleaned alias
// names nothing private while the raw spelling still names a directory the
// fallback JSON redacts in a separate section (#4938 review).
//
// The bare-blank boundary check matches any word-boundary occurrence, so a
// single-bare-word directory name also blanks the same word appearing in
// unrelated prose; the alternative the #4938 review rejected was shipping
// the verbatim private directory name through the daemon log section, which
// is the leak the fallback JSON redaction exists to prevent in a separate
// section. The over-blank in prose of a single private directory name is the
// privacy side of that fail-closed trade, mirroring the saturated scan's
// fail-closed over-blank of absolute paths past the slash-bearing cap.
func relativePathSpellings(path string) []string {
	cleaned := filepath.Clean(path)
	if cleaned == "" || filepath.IsAbs(cleaned) {
		return nil
	}
	if cleaned == "." || cleaned == ".." {
		// The cleaned form names nothing private, but the RAW spelling can:
		// a rejected record may carry a relative path such as
		// "ConfidentialClient/.." (or "ConfidentialClient/./..",
		// "a/b/../..") that filepath.Clean collapses to "." or "..", and
		// logVanishedWorktreeOnce emits that raw value via %q, so a
		// daemon-tail line such as repo_path="ConfidentialClient/.." ships
		// the private directory name verbatim even though the cleaned alias
		// is the harmless lone dot. Keep the raw spelling — it carries a
		// separator, so it routes to the slash-bearing log-only blank — so
		// the verbatim value does not survive the daemon log; skip the
		// cleaned alias, which names no private directory. A raw spelling
		// that already equals the cleaned trivial form (path is literally
		// "." or "..", or any no-separator spelling that Clean did not
		// rewrite) names nothing private and stays dropped (#4938 review).
		if path == cleaned || !strings.ContainsRune(path, filepath.Separator) {
			return nil
		}
		return []string{path}
	}
	spellings := []string{cleaned}
	if path != cleaned {
		spellings = append(spellings, path)
	}
	return spellings
}

// noteRoot registers one root under an exact token, reporting whether it was
// new. A path already registered keeps its FIRST token — two sessions in one
// repo must read as one repo, and an AF home that is also some session's repo
// must not gain a second name.
func (r *redactor) noteRoot(path, token string) bool {
	spellings := rootSpellings(path)
	if len(spellings) == 0 {
		return false
	}
	if r.rootTokens == nil {
		r.rootTokens = make(map[string]string)
	}
	for _, spelling := range spellings {
		if existing, seen := r.rootTokens[spelling]; seen {
			for _, alias := range spellings {
				r.noteRootSpelling(alias, existing)
			}
			return false
		}
	}
	for _, spelling := range spellings {
		r.noteRootSpelling(spelling, token)
	}
	return true
}

func (r *redactor) noteRootSpelling(path, token string) {
	if _, seen := r.rootTokens[path]; seen {
		return
	}
	r.rootTokens[path] = token
	r.roots = append(r.roots, pathRoot{path: path, token: token})
}

// rootSpellings returns both the spelling AF was handed and the physical
// spelling of its deepest existing ancestor. macOS commonly supplies /var paths
// while git and filepath resolution report the same objects below /private/var;
// Linux symlinked fixture roots have the identical shape. Registering both at
// admission keeps that filesystem alias from becoming an unrecognized text kind.
func rootSpellings(path string) []string {
	spellings := absolutePathSpellings(path)
	out := spellings[:0]
	for _, spelling := range spellings {
		if normalized := normalizeRoot(spelling); normalized != "" {
			out = append(out, normalized)
		}
	}
	return out
}

// absolutePathSpellings also admits the filesystem root. Registering "/" as a
// path root would consume every absolute path, so rootSpellings filters it; a
// repo at "/" can still own the exact sibling-title shape "/-<segment>".
func absolutePathSpellings(path string) []string {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil
	}
	spellings := []string{path}
	resolved := filepath.Clean(pathutil.ResolveForCompare(path))
	if filepath.IsAbs(resolved) && resolved != path {
		spellings = append(spellings, resolved)
	}
	return spellings
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
// unreachable. The text planner handles the same prefix-shadowing relationship
// for titles and usernames before it applies any replacements.
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
	return derivedWorktreePathBoundaryWithEnd(s, start, end, pathEndsAt)
}

func derivedWorktreePathBoundaryWithEnd(s string, start, end int, endsAt pathBoundary) bool {
	return derivedWorktreePathBoundaryWithContext(s, start, end, pathStartsAt, endsAt)
}

func derivedWorktreePathBoundaryWithContext(
	s string,
	start, end int,
	startsAt pathStartBoundary,
	endsAt pathBoundary,
) bool {
	if !startsAt(s, start) {
		return false
	}
	if endsAt(s, start, end) {
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
	return end > digits && endsAt(s, start, end)
}

func pathEndsAt(s string, start, end int) bool {
	if end == len(s) || s[end] == byte(filepath.Separator) {
		return true
	}
	after, size := utf8.DecodeRuneInString(s[end:])
	return isPathTextDelimiter(after) || uriPathEndsAt(s, start, end, size)
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
	return knownRootTextBoundaryWithEnd(s, start, end, pathEndsAt)
}

func knownRootTextBoundaryWithEnd(s string, start, end int, endsAt pathBoundary) bool {
	return knownRootTextBoundaryWithContext(s, start, end, pathStartsAt, endsAt)
}

func knownRootTextBoundaryWithContext(
	s string,
	start, end int,
	startsAt pathStartBoundary,
	endsAt pathBoundary,
) bool {
	if !startsAt(s, start) {
		return false
	}
	if endsAt(s, start, end) {
		return true
	}
	// This is the one safe non-separator sibling: an earlier structured or
	// contextual pass has already proven the suffix to be user-title data and
	// replaced it, so collapse the registered repo prefix while retaining both
	// the sibling dash and the marker (and any collision suffix).
	if strings.HasPrefix(s[end:], "-"+redactedMarker) {
		return true
	}
	return false
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
	_, ok := uriStartForPath(s, start)
	return ok
}

// uriPathEndsAt asks the URI parser whether the matched bytes are the complete
// path, or are followed by a percent-encoded path separator. That recognizes
// query, fragment, and encoded descendant syntax without declaring '?', '#', or
// '%' to be filesystem delimiters. All three are legal Unix filename bytes;
// outside a URI they may continue a sibling name and must not make a
// registered-root prefix eligible for collapse.
func uriPathEndsAt(s string, start, end, nextRuneSize int) bool {
	uriStart, ok := uriStartForPath(s, start)
	if !ok {
		return false
	}
	parseEnd := end + nextRuneSize
	if s[end] == '%' {
		if end+3 > len(s) {
			return false
		}
		parseEnd = end + 3
	}
	parsed, err := url.Parse(s[uriStart:parseEnd])
	if err != nil {
		return false
	}
	root := s[start:end]
	return parsed.Path == root || parsed.Path == root+"/"
}

// uriStartForPath locates a syntactically valid scheme whose first path slash
// is the match at start. The URI grammar permits both an authority
// (scheme://host/path) and no authority (scheme:/path); a path slash already
// present after the scheme/authority makes the match a suffix inside a longer
// URI path, never a root boundary.
func uriStartForPath(s string, start int) (int, bool) {
	prefix := s[:start]
	schemeEnd := strings.LastIndexByte(prefix, ':')
	if schemeEnd < 0 {
		return 0, false
	}
	pathPrefix := prefix[schemeEnd+1:]
	if pathPrefix != "" && (!strings.HasPrefix(pathPrefix, "//") || strings.Contains(pathPrefix[2:], "/")) {
		return 0, false
	}
	schemeStart := schemeEnd
	for schemeStart > 0 && isURISchemeByte(prefix[schemeStart-1]) {
		schemeStart--
	}
	if schemeStart == schemeEnd || !isASCIIAlpha(prefix[schemeStart]) {
		return 0, false
	}
	return schemeStart, true
}

func isURISchemeByte(b byte) bool {
	return isASCIIAlpha(b) || b >= '0' && b <= '9' || b == '+' || b == '-' || b == '.'
}

func isASCIIAlpha(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// The URI-path boundary pair: inside a percent-decoded URI path view, a
// registered path starts only at the view's start and ends only at a path
// separator or the view's end — the URI grammar's own delimiters, none of
// which are general filesystem text boundaries.
func uriWorktreePathBoundary(s string, start, end int) bool {
	return derivedWorktreePathBoundaryWithContext(s, start, end, uriPathStartsAt, uriLogicalPathEndsAt)
}

func uriKnownRootBoundary(s string, start, end int) bool {
	return knownRootTextBoundaryWithContext(s, start, end, uriPathStartsAt, uriLogicalPathEndsAt)
}

func uriPathStartsAt(_ string, start int) bool {
	return start == 0
}

func uriLogicalPathEndsAt(s string, _, end int) bool {
	return end == len(s) || end < len(s) && s[end] == '/'
}

// isPathTextDelimiter names punctuation and whitespace used by renderers around
// a complete path. Shell control operators and backtick wrappers terminate paths
// in command-bearing config values. Letters, numbers and filename punctuation
// such as '.', '_' and '-' deliberately do not qualify: they can continue a
// sibling's basename, which is the distinction collapsePathField's separator
// check makes for an isolated path value.
func isPathTextDelimiter(r rune) bool {
	// AF supports Unix filesystems, where NUL is the one byte that can never be
	// part of a pathname. NUL-delimited command output therefore proves a path
	// boundary on either side; it is not another renderer punctuation guess.
	if r == '\x00' {
		return true
	}
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
