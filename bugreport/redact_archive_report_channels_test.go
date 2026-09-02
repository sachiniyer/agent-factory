package bugreport

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// The #3541 leak is "the file names a user marked private escape a publicly
// shared bundle". #3546 closed the typed instances.json channel for
// RetainedTrees[].Skipped[].Path/PathBytes and stopped there. Three other
// channels carry the same names out of the same report, and each is covered
// below:
//
//  1. the generic fallback walk, which knows `path` but not `path_bytes`;
//  2. the retained tree's OWN PathBytes, which the typed pass leaves intact;
//  3. the bundled daemon log tail, which prints every skipped name verbatim.
//
// They share one property: base64 (1, 2) and a relative name (3) are both
// invisible to the text scrub, which can only collapse $HOME and a username.
// A channel the scrub cannot reach has to be closed structurally or not at all.

// TestRedactInstancesFallbackRedactsArchiveSkippedPathBytes is the #3541
// fallback guard. When any legacy or corrupt sibling field makes the typed
// []InstanceData decode fail, redactInstancesJSON switches to the generic
// key-driven walk — and that walk's sensitiveJSONKeys carried "path" but not
// "path_bytes". PathBytes is the DURABLE form of a name that is not valid
// UTF-8, marshaled as base64, so blanking only the display `path` left the
// private name in the bundle in its encoded form. base64 has no $HOME and no
// username in it, so the text scrub that backstops this walk cannot see it.
func TestRedactInstancesFallbackRedactsArchiveSkippedPathBytes(t *testing.T) {
	// An invalid-UTF-8 file name: the display form is lossy (U+FFFD), which is
	// exactly why PathBytes exists and exactly why it must be redacted too.
	rawName := "private-\xffwork.txt"
	encoded := base64.StdEncoding.EncodeToString([]byte(rawName))

	r := &redactor{}
	// `status` is a string here, which fails the typed decode and forces the
	// generic fallback — the same lever the #2419 and #3062 fallback guards use.
	raw := json.RawMessage(`[{"id":"leg-1","status":"legacy-string-status","program":"claude",` +
		`"archive_report":{"retained_trees":[{` +
		`"path":"/worktrees/.af-source-0123456789abcdef0123456789abcdef",` +
		`"skipped":[{"path":"private-�work.txt","path_bytes":"` + encoded + `","reason":"permission_denied"}]` +
		`}]}}]`)

	out := string(r.redactInstancesJSON(raw))

	if strings.Contains(out, encoded) {
		t.Errorf("fallback walk left the skipped path_bytes intact; the private name ships as base64:\n%s", out)
	}
	if !strings.Contains(out, redactedMarker) {
		t.Errorf("expected a redaction marker on the fallback path:\n%s", out)
	}
	// The fallback must stay structural: dropping the whole report would cost
	// triage the fact that the archive was incomplete.
	if !strings.Contains(out, "leg-1") || !strings.Contains(out, "permission_denied") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
}

// TestRedactInstanceDataClearsRetainedTreePathBytes is the #3541 guard for the
// retained tree's own raw path. The typed pass deliberately KEEPS
// RetainedTrees[].Path — it is the system worktree path, and the text scrub
// collapses $HOME to "~" and the username to "[user]" in it. That reasoning
// does not carry to PathBytes: when the root is not valid UTF-8 the field holds
// the complete raw ABSOLUTE path and marshals as base64, where neither the home
// directory nor the username is recognizable to the scrub. The supposedly
// redacted bundle then ships the operator's home path and username encoded.
func TestRedactInstanceDataClearsRetainedTreePathBytes(t *testing.T) {
	rawTree := "/home/alice/.agent-factory/worktrees/priv\xffate-source"
	d := session.InstanceData{
		ID:      "s-1",
		Program: "claude",
		Status:  session.Status(1),
		ArchiveReport: &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
			Path:      strings.ToValidUTF8(rawTree, "�"),
			PathBytes: []byte(rawTree),
			Skipped: []sessiongit.ArchiveSkippedEntry{{
				Path: "credential", Reason: sessiongit.ArchiveSkipPermissionDenied,
			}},
		}}},
	}

	redactInstanceData(&d)

	if got := d.ArchiveReport.RetainedTrees[0].PathBytes; len(got) != 0 {
		t.Errorf("retained tree path_bytes not cleared: %x — the raw absolute path survives the display redaction as base64", got)
	}
	// The display path still survives for the text scrub to collapse; clearing
	// the raw bytes must not cost triage the tree's location.
	if d.ArchiveReport.RetainedTrees[0].Path == "" {
		t.Error("retained tree display path was dropped; only the unscrubbable raw form should go")
	}
}

// TestRedactInstanceDataClearedTreePathBytesSurviveMarshal pins the property the
// clear actually depends on. ArchiveRetainedTree.MarshalJSON runs clone(), which
// RE-DERIVES PathBytes from Path whenever the field is empty. Clearing the field
// is therefore only a fix while the surviving display Path is valid UTF-8 — which
// it is, because archivePathFields already replaced the invalid bytes with U+FFFD.
// Without this guard a later change to that derivation would silently re-inflate
// the raw path on the way out and reopen the leak with every test still green.
func TestRedactInstanceDataClearedTreePathBytesSurviveMarshal(t *testing.T) {
	rawTree := "/home/alice/.agent-factory/worktrees/priv\xffate-source"
	encoded := base64.StdEncoding.EncodeToString([]byte(rawTree))
	d := session.InstanceData{
		ID:      "s-1",
		Program: "claude",
		Status:  session.Status(1),
		ArchiveReport: &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
			Path:      strings.ToValidUTF8(rawTree, "�"),
			PathBytes: []byte(rawTree),
			Skipped: []sessiongit.ArchiveSkippedEntry{{
				Path:      "private-�work.txt",
				PathBytes: []byte("private-\xffwork.txt"),
				Reason:    sessiongit.ArchiveSkipPermissionDenied,
			}},
		}}},
	}

	redactInstanceData(&d)
	out, err := json.Marshal(d.ArchiveReport)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), encoded) {
		t.Errorf("re-marshal re-derived the raw tree path bytes:\n%s", out)
	}
	if strings.Contains(string(out), base64.StdEncoding.EncodeToString([]byte("private-\xffwork.txt"))) {
		t.Errorf("re-marshal re-derived the raw skipped path bytes:\n%s", out)
	}
}

// TestNoteSessionRecordsArchiveSkippedPathsForLogScrub is the #3541 guard for
// the bundled daemon log tail — the channel #3546 left wide open.
//
// A Lost session restored from an incomplete archive logs
// `report.Warning("restore")` (daemon/lostrestore.go), and that renderer prints
// EVERY skipped name with %q (session/git/worktree_archive_report.go). collectLog
// then bundles the tail. scrubLog only knows session titles and tmux names, and
// the skipped names are RELATIVE to the retained worktree root — no $HOME, no
// username — so scrub() has nothing to collapse and the names ship verbatim.
// This is the same structural gap noteSession closes for tmux names in #1584:
// the structured section is redacted while the log beside it is not.
func TestNoteSessionRecordsArchiveSkippedPathsForLogScrub(t *testing.T) {
	leaks := []string{"credential", "private-work.txt", "generated/private-019"}
	skipped := make([]sessiongit.ArchiveSkippedEntry, 0, len(leaks))
	for _, name := range leaks {
		skipped = append(skipped, sessiongit.ArchiveSkippedEntry{
			Path: name, Reason: sessiongit.ArchiveSkipPermissionDenied,
		})
	}
	report := &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: "/worktrees/.af-source-0123456789abcdef0123456789abcdef", Skipped: skipped,
	}}}

	r := &redactor{}
	r.noteSession(&session.InstanceData{Title: "Kingfisher", ArchiveReport: report})

	// The exact line daemon/lostrestore.go writes on a restore.
	got := r.scrubLog(`restored lost session "Kingfisher": ` + report.Warning("restore"))

	for _, name := range leaks {
		if strings.Contains(got, name) {
			t.Errorf("bundled log tail retained the skipped file name %q: %s", name, got)
		}
	}
	// The diagnostic itself must survive — triage needs to know the archive was
	// incomplete and why, just not what the files were called.
	if !strings.Contains(got, "incomplete archive") || !strings.Contains(got, "permission denied") {
		t.Errorf("log scrub destroyed the archive diagnostic: %s", got)
	}
}

// TestNoteSessionRecordsArchiveSkippedRawPathForLogScrub covers the invalid-UTF-8
// half of the same channel. The warning renderer resolves each entry through
// filesystemPath(), which returns the RAW PathBytes when they are present — so
// the log carries bytes that never appear in the display Path. Recording only
// the display form leaves the raw name in the tail.
func TestNoteSessionRecordsArchiveSkippedRawPathForLogScrub(t *testing.T) {
	rawName := "private-\xffwork.txt"
	report := &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: "/worktrees/.af-source-0123456789abcdef0123456789abcdef",
		Skipped: []sessiongit.ArchiveSkippedEntry{{
			Path:      strings.ToValidUTF8(rawName, "�"),
			PathBytes: []byte(rawName),
			Reason:    sessiongit.ArchiveSkipPermissionDenied,
		}},
	}}}

	r := &redactor{}
	r.noteSession(&session.InstanceData{Title: "Kingfisher", ArchiveReport: report})

	got := r.scrubLog(`restored lost session "Kingfisher": ` + report.Warning("restore"))

	if strings.Contains(got, "private-") {
		t.Errorf("bundled log tail retained the raw skipped file name: %q", got)
	}
}
