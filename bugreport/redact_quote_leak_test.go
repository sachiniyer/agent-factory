package bugreport

import (
	"fmt"
	"strings"
	"testing"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// TestLogOnlyPathBlanksFailClosedSaturatedUnquotedAbsolutePathWithQuote pins the
// #4938 review fix on the saturated scan's handling of a literal '"' inside an
// UNQUOTED %s absolute path. session/backend_local_respawn.go logs the workDir
// with %s, so a past-the-cap worktree such as /srv/Confidential"Client reaches
// the whole daemon-log record verbatim (quoteStructural) rather than a decoded
// %q scalar. The walkback stops at the structural closing quote of a preceding
// %q session field, so the path does not start the view; the forward scan then
// stopped at the literal '"' and the saturated continuation refused to extend
// across it (quoteStructural), blanking only "/srv/Confidential" and shipping
// the private '"Client' suffix. saturatedQuoteIsStructural now lets the scan
// cross a '"' flanked by path-legal bytes (path data) and stop only at a real
// value boundary, so the whole path blanks.
//
// Fail-first: with the unconditional '"' termination the scan stops at the
// literal quote and "Client" ships verbatim.
func TestLogOnlyPathBlanksFailClosedSaturatedUnquotedAbsolutePathWithQuote(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
	}
	if !r.logOnlyPathBlanksSaturated {
		t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations (cap %d)",
			maxLogOnlyPathBlanks+16, maxLogOnlyPathBlanks)
	}
	// The workDir is logged unquoted via %s, so its literal '"' is path data on
	// the whole-record view, not the structural terminator of a %q value.
	const path = `/srv/Confidential"Client`
	logLine := fmt.Sprintf(`recover: rebuilt missing worktree for session %q at %s from branch %s`,
		"fix-bug", path, "main")
	got := r.scrubLog(logLine)
	t.Logf("saturated scan with unquoted quote-bearing absolute path out:\n%s", got)
	for _, secret := range []string{path, "Confidential", "Client"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q past the saturated scan (unquoted quote-bearing absolute path):\n%s", secret, got)
		}
	}
	for _, want := range []string{"rebuilt", "missing"} {
		if !strings.Contains(got, want) {
			t.Errorf("fail-closed pass removed non-path triage value %q before the path:\n%s", want, got)
		}
	}
}

// TestSaturatedBareNameBlanksUnquotedNameWithEmbeddedQuote pins the #4938 review
// fix on appendSaturatedBareNameSpans's handling of a literal '"' inside an
// UNQUOTED %s bare worktree. A past-the-cap relative worktree such as
// Confidential"Client — NewGitWorktreeFromStorage accepts any nonempty path —
// is logged verbatim via %s by session/backend_local_respawn.go's "at %s"
// workDir. The scan used to treat that '"' as the structural opener of a %q
// value, blanking only "Confidential" and entering a phantom quoted region
// that skipped "Client" and everything after it, so the private suffix shipped
// verbatim. saturatedQuoteIsStructural now treats a '"' flanked by path-legal
// bytes as filename content, so the whole "Confidential\"Client" token blanks
// at its text boundary instead.
//
// Fail-first: with the naive '"' toggle the scan blanks only "Confidential" and
// "Client" (and the rest of the record) survives inside a phantom quote.
func TestSaturatedBareNameBlanksUnquotedNameWithEmbeddedQuote(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("bare-name-%d", i))
	}
	if r.logOnlyPathBareNamesSaturated {
		t.Fatalf("logOnlyPathBareNamesSaturated set before the cap was reached (cap %d)", maxLogOnlyPathBlanks)
	}
	const secret = `Confidential"Client`
	r.noteLogOnlyPathRedaction(secret)
	if !r.logOnlyPathBareNamesSaturated {
		t.Fatalf("logOnlyPathBareNamesSaturated not set after a new bare name past the cap (cap %d)", maxLogOnlyPathBlanks)
	}
	// The bare name is logged via %s (NOT %q), so its literal '"' is path data
	// on the whole-record view. The "branch %s" carries a '/' so the record is
	// '/'-bearing, the shape the earlier no-'/' whole-record blank missed.
	logLine := fmt.Sprintf(`recover: rebuilt missing worktree for session %q at %s from branch %s`,
		"fix-bug", secret, "feature/foo")
	got := r.scrubLog(logLine)
	t.Logf("unquoted quote-bearing bare-name-saturated scrubLog out:\n%s", got)
	for _, leaked := range []string{secret, "Confidential", "Client"} {
		if strings.Contains(got, leaked) {
			t.Errorf("scrubLog leaked past-the-cap unquoted bare name %q with an embedded quote:\n%s", leaked, got)
		}
	}
	if !strings.Contains(got, "/") {
		t.Errorf("scrubLog lost every '/' of the '/'-bearing record, the bare-name scan must preserve slash-bearing shape:\n%s", got)
	}
}

// TestLogOnlyPathBareNamesSaturatedFailsClosedSlashBearingRecoveryError pins the
// #4938 review fix on a past-the-cap bare name that reaches the daemon log
// inside a '/'-bearing decoded %q recover_error scalar. logVanishedWorktreeOnce
// logs restoreErr.Error() with %q, and a failed rebuild can wrap the original
// `stat <bare-name>` error with a branch ref such as feature/foo, so one decoded
// scalar carries both the dropped bare name and a '/'. The bare-name cap is
// saturated but the slash-bearing cap is not, so the slash-bearing saturated
// scan does not run, the no-'/' whole-scalar blank declined at the '/', and the
// per-needle pass had no entry for the dropped name — the bare name used to ship
// verbatim. The per-scalar bare-name fail-closed now blanks the bare-name-shaped
// (no '/') tokens in the '/'-bearing scalar too, leaving the '/'-bearing
// segments to the slash-bearing per-needle pass.
//
// Fail-first: with the '/'-bearing scalar falling through, the bare name ships.
func TestLogOnlyPathBareNamesSaturatedFailsClosedSlashBearingRecoveryError(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("bare-name-%d", i))
	}
	if r.logOnlyPathBareNamesSaturated {
		t.Fatalf("logOnlyPathBareNamesSaturated set before the cap was reached (cap %d)", maxLogOnlyPathBlanks)
	}
	const secret = "ConfidentialClient4097"
	r.noteLogOnlyPathRedaction(secret)
	if !r.logOnlyPathBareNamesSaturated {
		t.Fatalf("logOnlyPathBareNamesSaturated not set after a new bare name past the cap (cap %d)", maxLogOnlyPathBlanks)
	}
	// A wrapped rebuild error: the bare name sits beside a branch ref, so the
	// decoded recover_error scalar carries both the bare name and a '/'.
	recovery := fmt.Sprintf("stat %s: failed on branch feature/foo", secret)
	logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" recover_error=%q`, recovery)
	got := r.scrubLog(logLine)
	t.Logf("slash-bearing recovery error scrubLog out:\n%s", got)
	if strings.Contains(got, secret) {
		t.Errorf("scrubLog leaked past-the-cap bare name %q in a '/'-bearing recovery error:\n%s", secret, got)
	}
	// The branch ref's '/' survives: its segments are preceded by '/', so
	// pathStartsAt rejects them and the slash-bearing shape is not destroyed.
	if !strings.Contains(got, "/") {
		t.Errorf("scrubLog lost every '/' of the '/'-bearing recovery error:\n%s", got)
	}
}

// TestWorktreePathTitlesSaturatedFailsClosedBareRepoSibling pins the #4938
// review fix on the worktree-title cap's fail-closed for a BARE (single-segment
// relative) repo_path. noteFallbackWorktreeTitle drops past-cap (repo_path,
// title) pairs, and the slash-anchored saturated scan cannot reach a sibling
// spelling with no '/', so a past-the-cap "<bare-repo>-<title>" such as
// ConfidentialClient-private-title leaked: the bare-name per-needle blank
// rejects the repo prefix before the '-' (the dash is filename-legal, not a
// text boundary), and the dropped title pair cannot redact the suffix. When
// the fallback registered a bare repo path the same fail-closed bare-name scan
// the bare-name cap uses now runs on a title-cap saturation too, blanking the
// bare sibling shape as one no-'/' token. An absolute-repo title-cap
// saturation keeps the triage the slash-bearing scan leaves alone (no bare
// name was registered).
//
// Fail-first: with the slash-anchored-only fail-closed the bare sibling shape
// ships verbatim.
func TestWorktreePathTitlesSaturatedFailsClosedBareRepoSibling(t *testing.T) {
	const repoPath = "ConfidentialClient" // single-segment relative (bare)
	r := &redactor{}
	r.noteLogOnlyPathRedaction(repoPath)
	for i := 0; i < maxWorktreePathTitles+16; i++ {
		r.noteFallbackWorktreeTitle(repoPath, fmt.Sprintf("title-%d", i))
	}
	if !r.worktreePathTitlesSaturated {
		t.Fatalf("worktreePathTitlesSaturated not set after %d fallback registrations (cap %d)",
			maxWorktreePathTitles+16, maxWorktreePathTitles)
	}
	const pastCapTitle = "Private-overflow-secret"
	segment := sessiongit.DerivedWorktreePathTitleSegment(repoPath, pastCapTitle)
	if segment == "" {
		t.Fatalf("DerivedWorktreePathTitleSegment returned empty for %q", pastCapTitle)
	}
	sibling := repoPath + "-" + segment
	if _, ok := r.worktreePathTitles[worktreePathTitle{repoPath: repoPath, segment: segment}]; ok {
		t.Fatalf("past-the-cap title pair %q got registered past the cap", pastCapTitle)
	}
	logLine := fmt.Sprintf(`recover: rebuilt missing worktree for session %q at %s from branch %s`,
		"other-session", sibling, "main")
	got := r.scrubLog(logLine)
	t.Logf("bare-repo title-cap-saturated scrubLog out:\n%s", got)
	for _, secret := range []string{repoPath, segment, "ConfidentialClient"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q past the bare-repo title cap (fail-open):\n%s", secret, got)
		}
	}
	// The quoted session name survives: the fail-closed bare-name scan skips
	// inside a %q region, so a quote-bearing triage value the path does not
	// own is not destroyed.
	if !strings.Contains(got, "other-session") {
		t.Errorf("scrubLog blanked the quoted session name that the bare-name fail-closed must leave alone:\n%s", got)
	}
}

// TestLogOnlyPathBlanksFailClosedSaturatedUnquotedPathWithSpacePrefixedQuote
// pins the #4938 review fix on the saturated scan's handling of a literal '"'
// that a path-text delimiter (a space) precedes inside an UNQUOTED %s path. A
// legal Unix filename can carry a double quote, and
// session/backend_local_respawn.go logs the workDir with %s, so a past-the-cap
// worktree such as `/srv/Acme "Secret"Client` reaches the whole daemon-log
// record verbatim (quoteStructural) rather than a decoded %q scalar. The space
// before the '"' made saturatedQuoteIsStructural classify it as a structural
// %q opener (a path-text delimiter on one side), so saturatedPathContinuationEnd
// stopped there and blanked only "/srv/Acme ", shipping `Secret"Client` in the
// daemon tail; the quote transform could then also redact "Secret" as an
// unrelated scalar, but the surrounding filename content still named the
// private directory. (The finding's `/srv/Acme "Secret"Client/repo` spelling is
// incidentally saved by the second `/repo` anchor walking back through the
// non-structural close, but with no trailing slash — or a space inside the
// pair — the same mechanism leaks the suffix, which is what this test pins.)
// saturatedQuoteIsStructural now validates a '"' that a path-text delimiter
// precedes against a real Go %q field: it is structural only when its matching
// Go %q close is itself followed by a delimiter or view boundary, so a filename
// quote pair ("Secret") whose close is followed by more filename content
// ("Client") is path data and the saturated scan crosses the pair whole
// instead of stranding the suffix after the space-prefixed opener.
//
// Fail-first: with the adjacent-character classifier the scan stops at the
// space-prefixed '"' and `Secret"Client` ships verbatim.
func TestLogOnlyPathBlanksFailClosedSaturatedUnquotedPathWithSpacePrefixedQuote(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
	}
	if !r.logOnlyPathBlanksSaturated {
		t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations (cap %d)",
			maxLogOnlyPathBlanks+16, maxLogOnlyPathBlanks)
	}
	// The workDir is logged unquoted via %s, so its literal '"' is path data on
	// the whole-record view; the space inside the name precedes the '"' that the
	// adjacent-character classifier misread as a structural %q opener. No
	// trailing slash follows the quote pair, so the second-`/` rescue of the
	// /repo spelling does not apply and the suffix leaks without the fix.
	const path = `/srv/Acme "Secret"Client`
	logLine := fmt.Sprintf(`recover: rebuilt missing worktree for session %q at %s from branch %s`,
		"fix-bug", path, "main")
	got := r.scrubLog(logLine)
	t.Logf("saturated scan with space-prefixed filename quote out:\n%s", got)
	for _, secret := range []string{path, "Secret", "Acme", "Client"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q past the saturated scan (space-prefixed filename quote):\n%s", secret, got)
		}
	}
	for _, want := range []string{"rebuilt", "missing"} {
		if !strings.Contains(got, want) {
			t.Errorf("fail-closed pass removed non-path triage value %q before the path:\n%s", want, got)
		}
	}
}
