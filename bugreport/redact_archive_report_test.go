package bugreport

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// TestRedactInstanceDataRedactsArchiveReportSkippedPath is the #3171 regression
// guard for the typed path. ArchiveReport arrived with lossless archive storage
// (#3171, commit faf61611) AFTER this redaction policy was written, so
// redactInstanceData never learned about RetainedTrees[].Skipped[].Path. That
// field carries the RELATIVE file names a user chose for files af could not read
// (hence permission_denied) — "credential", "private-work.txt",
// "generated/private-019" — and relative paths contain neither $HOME nor a
// username, so the text scrub the policy relies on (see the comment at the top
// of redactInstanceData) cannot collapse them. They shipped verbatim in every
// publicly shared bundle's archive_report, the same leak class as the
// title-derived fields in #2419 (PendingHandoffMission) and #2776
// (PendingTabCleanup[].TmuxName): a field added after the policy was written
// passed through unredacted.
func TestRedactInstanceDataRedactsArchiveReportSkippedPath(t *testing.T) {
	const treePath = "/worktrees/.af-source-0123456789abcdef0123456789abcdef"
	// The realistic relative file names the codebase's own fixtures use for
	// unreadable-but-retained sources (session/git/worktree_archive_report_test.go
	// and session/git/repo_gone_cleanup_test.go).
	leaks := []string{"credential", "private-work.txt", "private/old", "generated/private-019"}
	skipped := make([]sessiongit.ArchiveSkippedEntry, 0, len(leaks))
	for _, name := range leaks {
		skipped = append(skipped, sessiongit.ArchiveSkippedEntry{
			Path: name, Reason: sessiongit.ArchiveSkipPermissionDenied,
		})
	}
	d := session.InstanceData{
		ID:      "abc123",
		Program: "claude",
		Status:  session.Status(1),
		ArchiveReport: &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
			Path:          treePath,
			IdentityKnown: true,
			Device:        1,
			Inode:         2,
			FileType:      0o040000,
			Skipped:       skipped,
		}}},
	}

	redactInstanceData(&d)

	if d.ArchiveReport == nil {
		t.Fatal("redactInstanceData dropped the whole ArchiveReport; it should redact only the skipped paths so triage can still see the archive was incomplete")
	}
	tree := d.ArchiveReport.RetainedTrees[0]
	// The retained tree's own path is the SYSTEM worktree path the text scrub
	// collapses via $HOME→~; the structured redactor must leave it for that pass,
	// exactly as it leaves Worktree.Path and Branch for the scrub.
	if tree.Path != treePath {
		t.Errorf("retained_trees[0].path was changed by the structured redactor: got %q want %q (it is a system path the text scrub should collapse, not a user file name)", tree.Path, treePath)
	}
	for j, entry := range tree.Skipped {
		if entry.Path != redactedMarker {
			t.Errorf("retained_trees[0].skipped[%d].path not redacted: %q", j, entry.Path)
		}
		if entry.PathBytes != nil {
			t.Errorf("retained_trees[0].skipped[%d].path_bytes not cleared: %x", j, entry.PathBytes)
		}
		// The reason is the diagnostic ("permission denied on N files") and must
		// survive redaction, the way ids and counts survive everywhere else here.
		if entry.Reason != sessiongit.ArchiveSkipPermissionDenied {
			t.Errorf("retained_trees[0].skipped[%d].reason should survive redaction: %q", j, entry.Reason)
		}
	}
	// Structural fields still survive, the same invariant every other guard here checks.
	if d.ID != "abc123" || d.Program != "claude" || d.Status != session.Status(1) {
		t.Errorf("structural fields mutated: %+v", d)
	}
}

// TestRedactInstanceDataClearsArchiveReportSkippedPathBytes is the #3171 guard
// for the invalid-UTF8 half. A filename that is not valid UTF-8 is stored with
// Path holding the replacement-character display form and PathBytes holding the
// raw bytes (the durable value, since encoding/json cannot preserve such a
// string). Redacting only Path would leave PathBytes — and thus the real name —
// in the bundle, so both must be cleared together.
func TestRedactInstanceDataClearsArchiveReportSkippedPathBytes(t *testing.T) {
	const invalidName = "private/credential-\xff"
	d := session.InstanceData{
		ID:      "badutf8",
		Program: "claude",
		Status:  session.Status(1),
		ArchiveReport: &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
			Path: "/worktrees/.af-source-fedcba9876543210fedcba9876543210",
			Skipped: []sessiongit.ArchiveSkippedEntry{{
				Path:      invalidName,
				PathBytes: []byte(invalidName),
				Reason:    sessiongit.ArchiveSkipPermissionDenied,
			}},
		}}},
	}

	redactInstanceData(&d)

	entry := d.ArchiveReport.RetainedTrees[0].Skipped[0]
	if entry.Path != redactedMarker {
		t.Errorf("invalid-UTF8 skipped path not redacted: %q", entry.Path)
	}
	if entry.PathBytes != nil {
		t.Errorf("invalid-UTF8 skipped path_bytes not cleared: %x — the durable raw name survived the display redaction", entry.PathBytes)
	}
	if entry.Reason != sessiongit.ArchiveSkipPermissionDenied {
		t.Errorf("invalid-UTF8 skip reason should survive redaction: %q", entry.Reason)
	}
}

// TestRedactInstanceDataKeepsArchiveReportNil is the no-panic / no-overreach
// guard: a record without an incomplete archive must pass through redaction
// untouched on this field, the way the nil-pointer branches above it do.
func TestRedactInstanceDataKeepsArchiveReportNil(t *testing.T) {
	d := session.InstanceData{
		ID:      "no-archive",
		Program: "claude",
		Status:  session.Status(1),
		Title:   "ConfidentialDeal",
	}

	redactInstanceData(&d)

	if d.ArchiveReport != nil {
		t.Errorf("ArchiveReport mutated from nil to %+v", d.ArchiveReport)
	}
	if d.Title != redactedMarker {
		t.Errorf("title redaction regressed when ArchiveReport is nil: %q", d.Title)
	}
}

// TestRedactInstancesJSONRedactsArchiveReportSkippedPath exercises the leak the
// way a bundle actually produces it: through the typed decode of instances.json
// (collectInstances → redactInstancesJSON), which is the path that succeeds for
// every well-formed record. The generic fallback already blanked `path` via
// sensitiveJSONKeys, so the typed path is where the defense was missing — the
// common case. Before the fix the realistic relative names below survived the
// re-marshal and the text scrub (no $HOME/username to collapse) and reached a
// publicly shared bundle verbatim.
func TestRedactInstancesJSONRedactsArchiveReportSkippedPath(t *testing.T) {
	leaks := []string{"credential", "private-work.txt", "private/old", "generated/private-019"}
	const treePath = "/worktrees/.af-source-0123456789abcdef0123456789abcdef"
	skipped := make([]sessiongit.ArchiveSkippedEntry, 0, len(leaks))
	for _, name := range leaks {
		skipped = append(skipped, sessiongit.ArchiveSkippedEntry{
			Path: name, Reason: sessiongit.ArchiveSkipPermissionDenied,
		})
	}
	row := session.InstanceData{
		ID:      "s-1",
		Program: "claude",
		Status:  session.Status(1),
		ArchiveReport: &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
			Path: treePath, Skipped: skipped,
		}}},
	}
	raw, err := json.Marshal([]session.InstanceData{row})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	r := &redactor{}
	out := string(r.redactInstancesJSON(raw))

	for _, name := range leaks {
		if strings.Contains(out, name) {
			t.Errorf("archive_report skipped path %q leaked through the JSON redaction path:\n%s", name, out)
		}
	}
	if !strings.Contains(out, redactedMarker) {
		t.Errorf("expected a redaction marker for the skipped paths:\n%s", out)
	}
	// The report survives so triage can see the archive was incomplete; only the
	// user-chosen names inside it are dropped.
	if !strings.Contains(out, "archive_report") {
		t.Errorf("archive_report section should survive redaction:\n%s", out)
	}
	if !strings.Contains(out, "permission_denied") {
		t.Errorf("skip reason should survive redaction:\n%s", out)
	}
	// The retained tree path is the system worktree path; the structured redactor
	// leaves it for the text scrub to collapse $HOME (no home set here, so it is
	// unchanged), and it must not be redacted as if it were a user file name.
	if !strings.Contains(out, treePath) {
		t.Errorf("retained tree system path should survive redaction:\n%s", out)
	}
}

// TestBugReportBuildRedactsArchiveReportSkippedPath runs the full bugreport.Build
// entrypoint that `af bug-report` calls (commands/bugreportcmd.go:90) against a
// real instances.json on disk, and asserts the RENDERED bundle text — the thing
// a user actually attaches to a public issue — does not carry the user file
// names. This is the top-of-pipeline guard: Build → collectInstances →
// redactInstancesJSON → redactInstanceData → scrub, plus the final text render.
func TestBugReportBuildRedactsArchiveReportSkippedPath(t *testing.T) {
	home := t.TempDir()
	afHome := filepath.Join(home, ".agent-factory")
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	const treePath = "/worktrees/.af-source-fedcba9876543210fedcba9876543210"
	// Names chosen to mirror sensitive user files (the exploit scenario in #3171).
	leaks := []string{"credential", ".env.production", "customer-ssns.csv", "id_rsa_backup"}
	skipped := make([]sessiongit.ArchiveSkippedEntry, 0, len(leaks))
	for _, name := range leaks {
		skipped = append(skipped, sessiongit.ArchiveSkippedEntry{
			Path: name, Reason: sessiongit.ArchiveSkipPermissionDenied,
		})
	}
	row := session.InstanceData{
		ID:      "build-1",
		Program: "claude",
		Status:  session.Status(1),
		ArchiveReport: &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
			Path: treePath, Skipped: skipped,
		}}},
	}
	rows, err := json.Marshal([]session.InstanceData{row})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances("buildrepo", rows); err != nil {
		t.Fatalf("save: %v", err)
	}

	result, err := Build(Inputs{
		AFVersion:   "test",
		GeneratedAt: "2026-08-31 00:00:00 -0000",
		BundlePath:  filepath.Join(home, "af-bug-report-test.txt"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Build never fails on a missing section, but surface collection errors so a
	// redaction miss is not hidden behind a collectInstances failure.
	for _, name := range leaks {
		if strings.Contains(result.Text, name) {
			t.Errorf("user file name %q leaked into the rendered bug-report bundle text:\n%s", name, result.Text)
		}
		if strings.Contains(string(result.JSON), name) {
			t.Errorf("user file name %q leaked into the rendered bug-report bundle JSON:\n%s", name, string(result.JSON))
		}
	}
	// The redaction marker is present for the skipped paths, and the report
	// survives so triage can see the archive was incomplete.
	if !strings.Contains(result.Text, redactedMarker) {
		t.Errorf("expected a redaction marker in the bundle text for the skipped paths")
	}
	if !strings.Contains(result.Text, "archive_report") {
		t.Errorf("archive_report section should survive in the bundle for triage")
	}
	if !strings.Contains(result.Text, "permission_denied") {
		t.Errorf("skip reason should survive in the bundle for triage")
	}
}

// ---------------------------------------------------------------------------
// The log half of #3541: the bundled daemon log tail.
//
// Every guard below builds its input from the REAL renderer
// (ArchiveReport.Warning) rather than from a hand-written string. What has to
// come out of the bundle is exactly what af puts into it, so a fixture that
// drifted from session/git's warningSuffix would keep passing while the bundle
// leaked — and these are also the drift alarm for the scrub itself, which keys
// on that renderer's format and nothing else.
// ---------------------------------------------------------------------------

// archiveWarningLog renders one daemon log line the way the daemon writes it:
// daemon/lostrestore.go logs `restored lost session %q: %s` with
// report.Warning("restore").
func archiveWarningLog(report *sessiongit.ArchiveReport, operation string) string {
	return "restored lost session \"kingfisher\": " + report.Warning(operation)
}

// archiveReportWithSkipped builds a one-tree report whose skipped entries carry
// the given relative names, through the same exported fields storage decodes
// an instances.json row into.
func archiveReportWithSkipped(root string, names ...string) *sessiongit.ArchiveReport {
	skipped := make([]sessiongit.ArchiveSkippedEntry, 0, len(names))
	for _, name := range names {
		skipped = append(skipped, sessiongit.ArchiveSkippedEntry{
			Path: name, Reason: sessiongit.ArchiveSkipPermissionDenied,
		})
	}
	return &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: root, IdentityKnown: true, Skipped: skipped,
	}}}
}

// TestScrubLogRedactsArchiveWarningWithNoInstanceState is the log half of the
// #3541 leak. The daemon PRINTS the skipped names: restoring a Lost session
// whose archive was incomplete logs report.Warning("restore"), whose renderer
// embeds every skipped entry as %q, and collectLog bundles that log tail.
// scrubLog knew only titles, tmux names, $HOME and usernames — none of which
// match a name relative to the retained worktree root — so the JSON section read
// "[redacted]" while the log beneath it printed "private/old" verbatim, in the
// same publicly shared bundle.
//
// The redactor here is given NO state at all: no noteSession, no titles, no
// report. That is the claim this scrub makes and a state-keyed one cannot. The
// log outlives the state a live report could supply, in two ways #3554's review
// named: an instances.json that fails the typed decode never reaches
// noteSession at all, and a session killed after the warning was written has
// neither report nor row left while its log line still sits in the tail.
func TestScrubLogRedactsArchiveWarningWithNoInstanceState(t *testing.T) {
	const treePath = "/worktrees/.af-source-0123456789abcdef0123456789abcdef"
	// The realistic relative file names the codebase's own fixtures use for
	// unreadable-but-retained sources.
	leaks := []string{"credential", "private-work.txt", "private/old", "generated/private-019"}
	report := archiveReportWithSkipped(treePath, leaks...)

	r := &redactor{}
	got := r.scrubLog(archiveWarningLog(report, "restore"))

	for _, name := range leaks {
		if strings.Contains(got, name) {
			t.Errorf("skipped file name %q survived the log scrub:\n%s", name, got)
		}
	}
	if !strings.Contains(got, strconv.Quote(redactedMarker)) {
		t.Errorf("expected the redaction marker in place of the skipped names:\n%s", got)
	}
	// The diagnostic must survive: triage still needs to know the archive was
	// incomplete, how many files it left behind, and why — exactly as the JSON
	// section keeps the skip reason.
	if !strings.Contains(got, "incomplete archive") {
		t.Errorf("log scrub dropped the incomplete-archive diagnostic:\n%s", got)
	}
	if !strings.Contains(got, "af skipped 4 unreadable files") {
		t.Errorf("log scrub dropped the skipped-file count:\n%s", got)
	}
	if !strings.Contains(got, "permission denied") {
		t.Errorf("log scrub dropped the skip reason:\n%s", got)
	}
	// The retained root is a SYSTEM path, kept here for the same reason
	// redactInstanceData keeps tree.Path: the scrub collapses $HOME in it. It is
	// valid UTF-8, so the display rewrite is the identity.
	if !strings.Contains(got, strconv.Quote(treePath)) {
		t.Errorf("retained tree system path should survive the log scrub:\n%s", got)
	}
}

// TestScrubLogRedactsInvalidUTF8ArchiveWarningPath is the same guard for a name
// that is not valid UTF-8. The renderer prints entry.FilesystemPath() — the RAW
// bytes when PathBytes is populated, not the replacement-character display form
// — so the log carries the real name in its %q escaping.
func TestScrubLogRedactsInvalidUTF8ArchiveWarningPath(t *testing.T) {
	const invalidName = "private/credential-\xff"
	entry := sessiongit.ArchiveSkippedEntry{
		Path:      strings.ToValidUTF8(invalidName, "�"),
		PathBytes: []byte(invalidName),
		Reason:    sessiongit.ArchiveSkipPermissionDenied,
	}
	report := &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path:    "/worktrees/.af-source-fedcba9876543210fedcba9876543210",
		Skipped: []sessiongit.ArchiveSkippedEntry{entry},
	}}}

	r := &redactor{}
	got := r.scrubLog(archiveWarningLog(report, "restore"))

	// Assert on the quoted form of what the renderer PRINTS — entry
	// .FilesystemPath(), read from the same exported accessor the renderer uses,
	// so this test cannot drift from it. A raw-byte assertion would pass
	// vacuously: %q never emits the raw byte, it emits the ASCII escape `\xff`.
	if strings.Contains(got, strconv.Quote(entry.FilesystemPath())) {
		t.Errorf("invalid-UTF8 skipped file name survived the log scrub:\n%s", got)
	}
	if strings.Contains(got, "credential") {
		t.Errorf("the valid-UTF8 head of the invalid name survived the log scrub:\n%s", got)
	}
	if !strings.Contains(got, "incomplete archive") {
		t.Errorf("log scrub dropped the incomplete-archive diagnostic:\n%s", got)
	}
}

// TestScrubLogRedactsArchiveWarningBeforeTitles pins the ORDER inside scrubLog,
// which #3554's review reported as a live leak against the state-keyed scrub:
// with title "secret" and skipped path "docs/secret-plan.txt", the title pass
// rewrote the log to "docs/[redacted]-plan.txt" first, and the exact lookup for
// the original path then matched nothing, leaving the rest of the user's file
// name in a public bundle.
//
// Matching the warning's SHAPE removes the whole quoted token, so the overlap
// cannot strand a suffix — but the order still matters, and more sharply than
// before: this scrub keys on the renderer's literal prose, and the title pass
// replaces bare tokens ANYWHERE, including inside that prose. The second title
// here is "paths", which occurs in the renderer's own "skipped paths:" label
// with a non-word rune on both sides — exactly what replaceBareToken rewrites.
// Run the title pass first and the anchor is gone before the archive pass ever
// looks for it, and every name in the list ships.
func TestScrubLogRedactsArchiveWarningBeforeTitles(t *testing.T) {
	report := archiveReportWithSkipped(
		"/worktrees/.af-source-0123456789abcdef0123456789abcdef",
		"docs/secret-plan.txt", "private/old",
	)
	r := &redactor{}
	r.noteTitle("secret")
	r.noteTitle("paths")

	got := r.scrubLog(archiveWarningLog(report, "restore"))

	for _, fragment := range []string{"docs/", "-plan.txt", "secret-plan", "private/old"} {
		if strings.Contains(got, fragment) {
			t.Errorf("title overlap stranded %q in the log:\n%s", fragment, got)
		}
	}
	if !strings.Contains(got, "permission denied") {
		t.Errorf("log scrub dropped the skip reason:\n%s", got)
	}
}

// TestScrubLogRewritesRetainedRootToDisplayPath covers the retained tree's OWN
// path in the log, the fifth finding on #3554. redactInstanceData keeps
// tree.Path deliberately — it is a system path, and the text scrub collapses
// $HOME in it — but the warning renders tree.filesystemPath() through %q, which
// is the RAW bytes when the root is not valid UTF-8. Nothing in the ordinary
// home/username pass can see through that escaping, so the raw root shipped.
//
// The log copy is rewritten to the SAME display spelling the JSON section
// carries, rather than redacted outright: one policy for one value, and in the
// ordinary all-valid-UTF-8 case the rewrite is byte-for-byte the identity, so
// nothing is lost from a bundle that was already safe.
func TestScrubLogRewritesRetainedRootToDisplayPath(t *testing.T) {
	// A root OUTSIDE the configured home — the case Codex named, where the
	// home-to-tilde collapse has nothing to match even after the escaping is gone.
	const invalidRoot = "/data/worktrees/.af-source-\xff-kingfisher"
	report := archiveReportWithSkipped(invalidRoot, "private-work.txt")
	report.RetainedTrees[0].PathBytes = []byte(invalidRoot)

	r := &redactor{home: "/home/tester", users: []string{"tester"}}
	got := r.scrubLog(archiveWarningLog(report, "restore"))

	if strings.Contains(got, strconv.Quote(invalidRoot)) {
		t.Errorf("the raw retained root survived the log scrub in its %%q form:\n%s", got)
	}
	if strings.Contains(got, invalidRoot) {
		t.Errorf("the raw retained root bytes survived the log scrub:\n%s", got)
	}
	if !strings.Contains(got, strconv.Quote(strings.ToValidUTF8(invalidRoot, "�"))) {
		t.Errorf("the retained root should survive as its display spelling, for triage:\n%s", got)
	}
	if strings.Contains(got, "private-work.txt") {
		t.Errorf("skipped file name survived beside the retained root:\n%s", got)
	}
}

// TestScrubLogCollapsesHomeInInvalidUTF8RetainedRoot is the other half of that
// finding: a root whose invalid byte is part of the HOME spelling. Rewriting the
// quoted root to its display form is only a real fix if the home collapse can
// then reach it, and it cannot match the raw home bytes against a display
// spelling — so scrub() collapses both spellings of $HOME.
func TestScrubLogCollapsesHomeInInvalidUTF8RetainedRoot(t *testing.T) {
	const invalidHome = "/home/jd\xffoe"
	root := invalidHome + "/.agent-factory/worktrees/.af-source-abc123"
	report := archiveReportWithSkipped(root, "credential")
	report.RetainedTrees[0].PathBytes = []byte(root)

	r := &redactor{home: invalidHome}
	got := r.scrubLog(archiveWarningLog(report, "restore"))

	if strings.Contains(got, "jd") {
		t.Errorf("the home directory survived the log scrub in some spelling:\n%s", got)
	}
	if !strings.Contains(got, strconv.Quote("~/.agent-factory/worktrees/.af-source-abc123")) {
		t.Errorf("the retained root should collapse to ~ for triage:\n%s", got)
	}
	if strings.Contains(got, "credential") {
		t.Errorf("skipped file name survived the log scrub:\n%s", got)
	}
}

// TestScrubLogRedactsBoundedArchiveWarningForms exercises the two forms the
// renderer only produces past its own limits: the "showing first N of M" label
// (more than maxArchiveWarningEntries skipped files) and the "and N more in
// archive_report" tail (more than maxArchiveWarningTrees retained trees). Both
// put text the scrub has to walk past INSIDE the list it is matching, so a
// grammar that only handles the small case would stop redacting exactly where a
// user has the most unreadable files.
func TestScrubLogRedactsBoundedArchiveWarningForms(t *testing.T) {
	report := &sessiongit.ArchiveReport{}
	for tree := 0; tree < 6; tree++ {
		names := make([]string, 0, 5)
		for file := 0; file < 5; file++ {
			names = append(names, fmt.Sprintf("private/secret-%d-%d.txt", tree, file))
		}
		built := archiveReportWithSkipped(fmt.Sprintf("/worktrees/.af-source-%02d", tree), names...)
		report.RetainedTrees = append(report.RetainedTrees, built.RetainedTrees[0])
	}

	r := &redactor{}
	got := r.scrubLog(archiveWarningLog(report, "restore"))

	if strings.Contains(got, "secret-") {
		t.Errorf("a skipped file name survived the bounded rendering:\n%s", got)
	}
	if !strings.Contains(got, "showing first 20 of 30") {
		t.Errorf("the bounded-list label should survive for triage:\n%s", got)
	}
	if !strings.Contains(got, "and 2 more in archive_report") {
		t.Errorf("the omitted-trees label should survive for triage:\n%s", got)
	}
}

// TestScrubLogRewritesRetainedRootWithNoSkippedEntries covers the empty list.
// A retained tree with nothing in Skipped renders "skipped paths: " followed by
// nothing at all, and the root still has to be rewritten — a grammar that
// demands at least one entry silently stops matching the whole clause and ships
// the raw root.
func TestScrubLogRewritesRetainedRootWithNoSkippedEntries(t *testing.T) {
	const invalidRoot = "/worktrees/.af-source-\xff-kingfisher"
	report := &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: strings.ToValidUTF8(invalidRoot, "�"), PathBytes: []byte(invalidRoot),
	}}}

	r := &redactor{}
	got := r.scrubLog(archiveWarningLog(report, "restore"))

	if strings.Contains(got, strconv.Quote(invalidRoot)) {
		t.Errorf("the raw retained root survived on an empty skipped list:\n%s", got)
	}
	if !strings.Contains(got, strconv.Quote(strings.ToValidUTF8(invalidRoot, "�"))) {
		t.Errorf("the retained root should survive as its display spelling:\n%s", got)
	}
}

// TestScrubLogRedactsQuoteBearingArchiveWarningNames uses names that spoof the
// renderer's own punctuation. A `"` and a `\` are both legal bytes in a unix
// filename, so a user can name a file — or a directory af retains — so that its
// printed form reads as the end of one list item and the start of another, or as
// the end of the whole clause. What keeps the scrub exact is that it reads the
// ESCAPING rather than the punctuation: strconv.Quote writes an interior quote
// as \" and an interior backslash as \\, so each entry is still exactly one
// token. A matcher that split on `", "` would cut a name in half and leave the
// remainder in a public bundle.
func TestScrubLogRedactsQuoteBearingArchiveWarningNames(t *testing.T) {
	root := `/worktrees/.af-source"; skipped paths: "decoy`
	report := archiveReportWithSkipped(root,
		`inner" (permission denied), "escape.txt`,
		`back\slash.txt`,
	)

	r := &redactor{}
	got := r.scrubLog(archiveWarningLog(report, "restore"))

	for _, fragment := range []string{"inner", "escape.txt", "back", "slash.txt"} {
		if strings.Contains(got, fragment) {
			t.Errorf("a quote-bearing skipped name left %q in the log:\n%s", fragment, got)
		}
	}
	// The retained root survives as ONE token, punctuation and all: the spoofed
	// "; skipped paths:" inside it is escaped text, not a clause boundary, and
	// reading it as one would have redacted the wrong half of this line.
	if !strings.Contains(got, strconv.Quote(root)) {
		t.Errorf("the retained root was not carried through as a single token:\n%s", got)
	}
	if !strings.Contains(got, "af skipped 2 unreadable files") {
		t.Errorf("log scrub dropped the skipped-file count:\n%s", got)
	}
}

// TestScrubLogRedactsUnrecognizedArchiveWarningTail pins the direction this
// scrub fails in. It reads the renderer's grammar, so anything that breaks that
// grammar before the bundle is built — a caller appending to the line, a future
// field added to the warning — makes the list unparseable. When that happens the
// whole list goes, diagnostics and all: a bundle that loses a reason is a
// nuisance, and one that ships a user's file names is the bug being fixed.
func TestScrubLogRedactsUnrecognizedArchiveWarningTail(t *testing.T) {
	leaks := []string{"credential", "private-work.txt"}
	report := archiveReportWithSkipped("/worktrees/.af-source-abc123", leaks...)

	r := &redactor{}
	got := r.scrubLog(archiveWarningLog(report, "restore") + " (attempt 2 of 3)")

	for _, name := range leaks {
		if strings.Contains(got, name) {
			t.Errorf("skipped file name %q survived a warning the grammar could not parse:\n%s", name, got)
		}
	}
	if !strings.Contains(got, redactedMarker) {
		t.Errorf("expected the redaction marker for the unparseable list:\n%s", got)
	}
	// The counted diagnostic sits before the retained-tree clause, so it survives
	// even when the list itself cannot be parsed.
	if !strings.Contains(got, "af skipped 2 unreadable files") {
		t.Errorf("the skipped-file count should survive an unparseable list:\n%s", got)
	}
}

// TestRedactInstanceDataClearsRetainedTreePathBytes covers the retained tree's
// OWN path. The structured policy deliberately keeps tree.Path so the text scrub
// can collapse $HOME to "~" — but when the root is not valid UTF-8, PathBytes
// holds the complete raw absolute path and encoding/json emits it as BASE64. The
// text scrub cannot see a home directory or a username through base64, so the
// raw root path shipped in a bundle that had otherwise been redacted.
func TestRedactInstanceDataClearsRetainedTreePathBytes(t *testing.T) {
	const invalidRoot = "/worktrees/.af-source-\xff-kingfisher"
	d := session.InstanceData{
		ID: "badroot",
		ArchiveReport: &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
			Path:          strings.ToValidUTF8(invalidRoot, "�"),
			PathBytes:     []byte(invalidRoot),
			IdentityKnown: true,
			Skipped: []sessiongit.ArchiveSkippedEntry{{
				Path: "private-work.txt", Reason: sessiongit.ArchiveSkipPermissionDenied,
			}},
		}}},
	}

	redactInstanceData(&d)

	tree := d.ArchiveReport.RetainedTrees[0]
	if tree.PathBytes != nil {
		t.Errorf("retained_trees[0].path_bytes not cleared: %x — the raw root path survives base64-encoded, where the text scrub cannot reach it", tree.PathBytes)
	}
	// Re-marshaling is what actually ships. ArchiveRetainedTree.MarshalJSON
	// re-derives PathBytes from Path when it is empty, so the assertion above is
	// only half the claim: prove the wire form carries no raw bytes either.
	out, err := json.Marshal(d.ArchiveReport)
	if err != nil {
		t.Fatalf("marshal redacted report: %v", err)
	}
	if strings.Contains(string(out), "path_bytes") {
		t.Errorf("redacted report still emits path_bytes on the wire:\n%s", out)
	}
	// The display path still survives for triage — it is the system worktree path
	// the text scrub collapses, not a user file name.
	if tree.Path == "" || tree.Path == redactedMarker {
		t.Errorf("retained_trees[0].path should survive for the text scrub, got %q", tree.Path)
	}
}

// TestRedactUnknownJSONRedactsPathBytes is the generic-fallback half. When any
// legacy or corrupt sibling field makes the typed []InstanceData decode fail,
// redactInstanceData never runs and redactInstancesJSON switches to the
// key-driven walk. That walk knew "path" but not "path_bytes", so an
// invalid-UTF8 skipped file name still shipped as its base64-encoded raw bytes —
// which the closing text scrub cannot recognize as anything at all.
func TestRedactUnknownJSONRedactsPathBytes(t *testing.T) {
	const invalidName = "private/credential-\xff"
	raw := []byte(`{"archive_report":{"retained_trees":[{"path":"/worktrees/.af-source-0","path_bytes":"` +
		base64.StdEncoding.EncodeToString([]byte(invalidName)) +
		`","skipped":[{"path":"private/old","path_bytes":"` +
		base64.StdEncoding.EncodeToString([]byte(invalidName)) +
		`","reason":"permission_denied"}]}]}}`)
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}

	out, err := json.Marshal(redactUnknownJSON(generic))
	if err != nil {
		t.Fatalf("marshal generic redaction: %v", err)
	}

	// The base64 payload is the leak: it is the raw name, and no text scrub can
	// see through it. Assert on the encoded form the walk actually emits.
	if strings.Contains(string(out), base64.StdEncoding.EncodeToString([]byte(invalidName))) {
		t.Errorf("generic fallback kept path_bytes, shipping the raw file name base64-encoded:\n%s", out)
	}
	if strings.Contains(string(out), "private/old") {
		t.Errorf("generic fallback kept the display path:\n%s", out)
	}
	// The skip reason is structural and must survive, as it does on the typed path.
	if !strings.Contains(string(out), "permission_denied") {
		t.Errorf("generic fallback dropped the structural skip reason:\n%s", out)
	}
}

// TestRedactInstanceDataRetainedTreePathBytesStayClearedOnMarshal proves the
// clearing above actually holds on the wire. ArchiveRetainedTree.MarshalJSON
// re-derives PathBytes from Path whenever it is empty, so a record whose Path
// still carries invalid UTF-8 would put the raw bytes back the moment the
// bundle is marshaled — nil at the end of redactInstanceData is not the same
// claim as absent from the bundle.
func TestRedactInstanceDataRetainedTreePathBytesStayClearedOnMarshal(t *testing.T) {
	const invalidRoot = "/worktrees/.af-source-\xff-kingfisher"
	d := session.InstanceData{
		ID: "rederive",
		// Path invalid and PathBytes already empty: the shape MarshalJSON
		// re-derives from.
		ArchiveReport: &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
			Path: invalidRoot,
			Skipped: []sessiongit.ArchiveSkippedEntry{{
				Path: "private-work.txt", Reason: sessiongit.ArchiveSkipPermissionDenied,
			}},
		}}},
	}

	redactInstanceData(&d)

	out, err := json.Marshal(d.ArchiveReport)
	if err != nil {
		t.Fatalf("marshal redacted report: %v", err)
	}
	if strings.Contains(string(out), "path_bytes") {
		t.Errorf("MarshalJSON re-derived path_bytes from an invalid-UTF8 path:\n%s", out)
	}
	if strings.Contains(string(out), base64.StdEncoding.EncodeToString([]byte(invalidRoot))) {
		t.Errorf("raw root path shipped base64-encoded:\n%s", out)
	}
}
