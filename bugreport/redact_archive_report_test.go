package bugreport

import (
	"encoding/base64"
	"encoding/json"
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

// TestNoteSessionRecordsArchiveReportSkippedPathsForTheLog is the log half of
// the #3541 leak, and the half that shipped unclosed. redactInstanceData blanks
// RetainedTrees[].Skipped[].Path in the JSON section, but the daemon PRINTS the
// same names: a Lost session restored with an incomplete archive logs
// report.Warning("restore") (daemon/lostrestore.go), whose renderer embeds every
// skipped entry's filesystem path as %q. collectLog bundles that log tail, and
// scrubLog only knew about titles, tmux names, $HOME and usernames — none of
// which match a name relative to the retained worktree root. So the structured
// section said "[redacted]" while the log beneath it printed "private/old"
// verbatim, in the same publicly shared bundle.
//
// The log line is built from the REAL renderer rather than a hand-written
// string: what has to come out of the log is exactly what the daemon puts into
// it, and a fixture that drifts from the renderer would pass while the bundle
// still leaked.
func TestNoteSessionRecordsArchiveReportSkippedPathsForTheLog(t *testing.T) {
	leaks := []string{"credential", "private-work.txt", "private/old", "generated/private-019"}
	skipped := make([]sessiongit.ArchiveSkippedEntry, 0, len(leaks))
	for _, name := range leaks {
		skipped = append(skipped, sessiongit.ArchiveSkippedEntry{
			Path: name, Reason: sessiongit.ArchiveSkipPermissionDenied,
		})
	}
	report := &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path:          "/worktrees/.af-source-0123456789abcdef0123456789abcdef",
		IdentityKnown: true,
		Skipped:       skipped,
	}}}
	d := session.InstanceData{ID: "abc123", Program: "claude", ArchiveReport: report}

	r := &redactor{}
	r.noteSession(&d)

	// The exact shape daemon/lostrestore.go writes to the production log.
	line := "restored lost session \"kingfisher\": " + report.Warning("restore")
	got := r.scrubLog(line)

	for _, name := range leaks {
		if strings.Contains(got, name) {
			t.Errorf("skipped file name %q survived the log scrub:\n%s", name, got)
		}
	}
	// The diagnostic must survive: triage still needs to know the archive was
	// incomplete and why, exactly as the JSON section keeps the skip reason.
	if !strings.Contains(got, "incomplete archive") {
		t.Errorf("log scrub dropped the incomplete-archive diagnostic:\n%s", got)
	}
	if !strings.Contains(got, "permission denied") {
		t.Errorf("log scrub dropped the skip reason:\n%s", got)
	}
}

// TestNoteSessionRecordsInvalidUTF8SkippedPathForTheLog is the same guard for a
// name that is not valid UTF-8. The renderer prints entry.FilesystemPath(),
// which is the RAW bytes when PathBytes is populated — not the
// replacement-character display form — so recording the display string alone
// would leave the real name in the log.
func TestNoteSessionRecordsInvalidUTF8SkippedPathForTheLog(t *testing.T) {
	const invalidName = "private/credential-\xff"
	report := &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: "/worktrees/.af-source-fedcba9876543210fedcba9876543210",
		Skipped: []sessiongit.ArchiveSkippedEntry{{
			Path:      strings.ToValidUTF8(invalidName, "�"),
			PathBytes: []byte(invalidName),
			Reason:    sessiongit.ArchiveSkipPermissionDenied,
		}},
	}}}
	d := session.InstanceData{ID: "badutf8", ArchiveReport: report}

	r := &redactor{}
	r.noteSession(&d)

	got := r.scrubLog("restored lost session \"kingfisher\": " + report.Warning("restore"))

	// Assert on strconv.Quote(invalidName), not the raw string: the renderer
	// prints the path with %q, so the invalid byte reaches the log as the ASCII
	// escape `\xff` and a raw-byte assertion could never match — it would pass
	// while the escaped name sat in the bundle.
	if strings.Contains(got, strconv.Quote(invalidName)) {
		t.Errorf("invalid-UTF8 skipped file name survived the log scrub:\n%s", got)
	}
	if !strings.Contains(got, "incomplete archive") {
		t.Errorf("log scrub dropped the incomplete-archive diagnostic:\n%s", got)
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
