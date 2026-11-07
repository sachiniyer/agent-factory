package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// These tests pin the snapshot-MISS branch of getSessionByTitle: when the
// daemon's live snapshot lacks the title but the disk scan (findInstanceByTitle)
// positively resolves it, the same incomplete-widening caveat the snapshot-MATCH
// branch applies must also surface — naming the corrupted/unreadable sibling
// repos the scan could not check, on BOTH delivery channels (stderr text and the
// --json warnings payload), while still resolving the row (#3479/#3511/#6882).
//
// They are the miss-branch twins of the match-branch tests in
// unreadable_reporting_test.go. Before the fix the miss branch's positive
// resolution returned a hardcoded empty notice, so the caveat was silently
// dropped on this path even though the branch's own comment claimed it asked "the
// same question the match branch's widening asks."

// stubSnapshotMiss sets up a live daemon snapshot that does NOT contain title, so
// getSessionByTitle falls through the match branch to the disk-scan miss branch.
// The snapshot must succeed (nil error) with data that lacks the target title.
func stubSnapshotMiss(t *testing.T, title string) {
	t.Helper()
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: "/repos/live"}}, nil
	})
}

// TestGetSessionByTitle_SnapshotMissReportsIncompleteWideningOnCorruptedRepo is
// the miss-branch twin of TestGetSessionByTitle_ReportsIncompleteWideningOnCorruptedRepo.
// The snapshot misses the title; the disk scan finds it in a healthy repo while a
// sibling repo's instances.json is parse-corrupted. The positive resolution must
// still caveat naming the corrupted file — on stderr AND in the returned notice
// for --json callers — rather than reading as an exhaustive, confident answer.
func TestGetSessionByTitle_SnapshotMissReportsIncompleteWideningOnCorruptedRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	warnLog := captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	seedRows(t, "main-repo", session.InstanceData{Title: "foo", Path: "/repos/main"})
	if err := config.SaveRepoInstances("corrupt-repo", json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupt-repo: %v", err)
	}
	path, err := config.RepoInstancesPath("corrupt-repo")
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}

	stubSnapshotMiss(t, "foo")

	got, notice, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a read must not break because its ambiguity widening hit a corrupted file: %v", err)
	}
	if got == nil || got.Path != "/repos/main" {
		t.Fatalf("expected the disk match to resolve, got: %+v", got)
	}
	if !strings.Contains(warn.String(), "could not check every project") {
		t.Errorf("an incomplete widening must be reported, not silently dropped; stderr was: %q", warn.String())
	}
	if !strings.Contains(warn.String(), path) {
		t.Errorf("the notice must name the corrupted file; stderr was: %q", warn.String())
	}
	if !strings.Contains(warn.String(), "--repo") {
		t.Errorf("the notice should point at the scope that would settle it; stderr was: %q", warn.String())
	}
	if notice == "" {
		t.Errorf("the notice must also be returned, for --json callers to carry (#3511)")
	}
	if !strings.Contains(notice, path) {
		t.Errorf("the returned notice must name the corrupted file too; got: %q", notice)
	}
	// A parse-corrupted instances.json must not advise removing it (af keeps no
	// backup); the corruption-safe repair hint must be carried instead (#5530).
	if strings.Contains(notice, "remove the file") {
		t.Errorf("the notice for a corrupted file must not advise removing it; got: %q", notice)
	}
	if !strings.Contains(notice, "copy the corrupt") {
		t.Errorf("the notice for a corrupted file must carry the corruption-safe repair hint; got: %q", notice)
	}
	if !strings.Contains(warnLog.String(), "corrupt-repo") {
		t.Errorf("the corrupted repo must be logged while widening; warning log was: %q", warnLog.String())
	}
}

// TestGetSessionByTitle_SnapshotMissReportsIncompleteWideningOnUnreadableRepo is
// the miss-branch twin of TestGetSessionByTitle_ReportsIncompleteWideningButStillResolves.
// The snapshot misses the title; the disk scan finds it in a healthy repo while a
// sibling repo's instances.json is unreadable. The positive resolution must still
// caveat naming the unreadable file.
func TestGetSessionByTitle_SnapshotMissReportsIncompleteWideningOnUnreadableRepo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based read-failure injection is bypassed when running as root")
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	seedRows(t, "main-repo", session.InstanceData{Title: "foo", Path: "/repos/main"})
	seedRows(t, "blocked-repo", session.InstanceData{Title: "other", Path: "/repos/blocked"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	stubSnapshotMiss(t, "foo")

	got, notice, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a read must not break because its ambiguity widening was incomplete: %v", err)
	}
	if got == nil || got.Path != "/repos/main" {
		t.Fatalf("expected the disk match to resolve, got: %+v", got)
	}
	if !strings.Contains(warn.String(), path) {
		t.Errorf("an incomplete widening must be reported, naming the file; stderr was: %q", warn.String())
	}
	if !strings.Contains(warn.String(), "--repo") {
		t.Errorf("the notice should point at the scope that would settle it; stderr was: %q", warn.String())
	}
	if notice == "" {
		t.Errorf("the notice must also be returned, for --json callers to carry (#3511)")
	}
	if !strings.Contains(notice, path) {
		t.Errorf("the returned notice must name the unreadable file too; got: %q", notice)
	}
}

// TestGetSessionByTitle_SnapshotMissStillResolvesWithCorruptedSibling pins the
// #3507 presence contract on the miss branch: a positive disk resolution is not
// weakened by a file the scan could not read. The healthy repo's session must
// still resolve — the caveat is reported, not refused.
func TestGetSessionByTitle_SnapshotMissStillResolvesWithCorruptedSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	_ = captureWarnWriter(t)
	resetIncompleteNotices(t)

	seedRows(t, "main-repo", session.InstanceData{Title: "foo", Path: "/repos/main", Liveness: session.LiveReady})
	if err := config.SaveRepoInstances("corrupt-repo", json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupt-repo: %v", err)
	}

	stubSnapshotMiss(t, "foo")

	got, _, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a presence this scan can see must not be weakened by a file it could not read (#3507): %v", err)
	}
	if got == nil || got.Title != "foo" || got.Path != "/repos/main" {
		t.Fatalf("expected the healthy repo's session to resolve, got: %+v", got)
	}
}

// TestGetSessionByTitle_SnapshotMissLivenessCoercedToLost confirms the positive
// resolution still flows through unconfirmedLiveProjection: a disk-only row's
// persisted LiveReady/LiveRunning claim is coerced to LiveLost because the
// running daemon's snapshot could not confirm it (#6882 review). The caveat must
// not interfere with the liveness projection.
func TestGetSessionByTitle_SnapshotMissLivenessCoercedToLost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	_ = captureWarnWriter(t)
	resetIncompleteNotices(t)

	saveOneRepoInstances(t, "repo-alpha", session.InstanceData{
		Title: "ghost", Path: "/repos/alpha", BackendType: "docker", Liveness: session.LiveReady,
	})
	if err := config.SaveRepoInstances("corrupt-repo", json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupt-repo: %v", err)
	}

	stubSnapshotMiss(t, "ghost")

	got, notice, err := getSessionByTitle("ghost")
	if err != nil {
		t.Fatalf("a disk-only row must resolve despite the snapshot miss: %v", err)
	}
	if got.Liveness != session.LiveLost {
		t.Fatalf("a disk-only row's live liveness must project as LiveLost, got %v", got.Liveness)
	}
	// The caveat must still surface alongside the liveness coercion.
	if notice == "" {
		t.Errorf("the incomplete-widening notice must surface even when liveness is coerced; got empty notice")
	}
}

// TestGetSessionByTitle_SnapshotMissJSONCarriesCorruptedWideningNotice is the
// miss-branch twin of TestGetSessionByTitleInScope_JSONCarriesCorruptedWideningNotice:
// under --json the corrupted-widening notice must ride inside the envelope's
// `data` payload (resolved session + additive `warnings` array), because stderr
// under --json carries only the {data,error} envelope and drops a free-form line
// (#3169). Before the fix the miss branch produced NO notice at all, so --json
// resolved clean with no warnings array — the silent wrong-project resolution.
func TestGetSessionByTitle_SnapshotMissJSONCarriesCorruptedWideningNotice(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	seedRows(t, "main-repo", session.InstanceData{Title: "foo", Path: "/repos/main"})
	if err := config.SaveRepoInstances("corrupt-repo", json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupt-repo: %v", err)
	}
	path, err := config.RepoInstancesPath("corrupt-repo")
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}

	withEnvelope(t, func() {
		stubSnapshotMiss(t, "foo")

		data, notice, err := getSessionByTitle("foo")
		if err != nil {
			t.Fatalf("a read must not break because its widening hit a corrupted file: %v", err)
		}

		out := captureStdout(t, func() {
			if err := jsonOut(sessionGetPayload(data, notice)); err != nil {
				t.Fatalf("jsonOut: %v", err)
			}
		})

		var envelope struct {
			Data struct {
				Path     string   `json:"path"`
				Warnings []string `json:"warnings"`
			} `json:"data"`
			Error *string `json:"error"`
		}
		if err := json.Unmarshal([]byte(out), &envelope); err != nil {
			t.Fatalf("stdout was not the expected envelope shape: %v\nstdout: %s", err, out)
		}
		if envelope.Data.Path != "/repos/main" {
			t.Fatalf("expected the disk match to resolve, got: %+v", envelope.Data)
		}
		if len(envelope.Data.Warnings) != 1 {
			t.Fatalf("expected exactly one warning embedded in data, got: %+v", envelope.Data.Warnings)
		}
		if !strings.Contains(envelope.Data.Warnings[0], path) {
			t.Errorf("the embedded warning must name the corrupted file %s, got: %q", path, envelope.Data.Warnings[0])
		}
		if warn.String() != "" {
			t.Errorf("under --json, stderr must carry nothing — the notice belongs in data, not stderr; stderr was: %q", warn.String())
		}
	})
}

// TestGetSessionByTitle_SnapshotMissNoticeSaidOnce is the miss-branch twin of
// TestIncompleteWideningNoticeIsSaidOnce: an unscoped `sessions watch` re-resolves
// the title every poll (up to ~900 times at the default timeout), and the miss
// branch's caveat must be deduped via noteOnce so it is said once, not once per
// poll.
func TestGetSessionByTitle_SnapshotMissNoticeSaidOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based read-failure injection is bypassed when running as root")
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	seedRows(t, "main-repo", session.InstanceData{Title: "foo", Path: "/repos/main"})
	seedRows(t, "blocked-repo", session.InstanceData{Title: "other", Path: "/repos/blocked"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	stubSnapshotMiss(t, "foo")

	for i := 0; i < 25; i++ {
		if _, _, err := getSessionByTitle("foo"); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if got := strings.Count(warn.String(), "warning: could not check every project"); got != 1 {
		t.Fatalf("the caveat must be said once per invocation, not once per poll; said it %d times", got)
	}
	if !strings.Contains(warn.String(), path) {
		t.Errorf("the one notice must still name the unreadable file; stderr was: %q", warn.String())
	}
}

// TestGetSessionByTitle_SnapshotMissCleanResolutionHasNoNotice is the no-regression
// guard: when the disk scan is clean (no corrupted/unreadable siblings), the miss
// branch's positive resolution must return an empty notice — no spurious caveat.
func TestGetSessionByTitle_SnapshotMissCleanResolutionHasNoNotice(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	seedRows(t, "main-repo", session.InstanceData{Title: "foo", Path: "/repos/main"})

	stubSnapshotMiss(t, "foo")

	got, notice, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a clean disk resolution must not error: %v", err)
	}
	if got == nil || got.Path != "/repos/main" {
		t.Fatalf("expected the disk match to resolve, got: %+v", got)
	}
	if notice != "" {
		t.Errorf("a clean widening must return an empty notice; got: %q", notice)
	}
	if warn.String() != "" {
		t.Errorf("a clean widening must print nothing to stderr; stderr was: %q", warn.String())
	}
}

// TestGetSessionByTitle_SnapshotMissCorruptedAndUnreadableWideningBothReported is
// the miss-branch twin of TestGetSessionByTitle_CorruptedAndUnreadableWideningBothReported:
// both gap classes (unreadable + corrupted) must surface in the one notice, in
// stable repoID order.
func TestGetSessionByTitle_SnapshotMissCorruptedAndUnreadableWideningBothReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based read-failure injection is bypassed when running as root")
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	seedRows(t, "main-repo", session.InstanceData{Title: "foo", Path: "/repos/main"})
	// zeta-unreadable sorts AFTER alpha-corrupted by repoID, so the order
	// assertion below pins both the "both reported" and the stable-order
	// contracts at once.
	seedRows(t, "zeta-unreadable", session.InstanceData{Title: "other", Path: "/repos/zeta"})
	makeRecordsUnreadable(t, "zeta-unreadable")
	unreadablePath, err := config.RepoInstancesPath("zeta-unreadable")
	if err != nil {
		t.Fatalf("RepoInstancesPath(zeta): %v", err)
	}
	if err := config.SaveRepoInstances("alpha-corrupted", json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save alpha-corrupted: %v", err)
	}
	corruptedPath, err := config.RepoInstancesPath("alpha-corrupted")
	if err != nil {
		t.Fatalf("RepoInstancesPath(alpha): %v", err)
	}

	stubSnapshotMiss(t, "foo")

	got, notice, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("the read must still resolve with both gap classes present: %v", err)
	}
	if got == nil || got.Path != "/repos/main" {
		t.Fatalf("expected the disk match to resolve, got: %+v", got)
	}
	if !strings.Contains(notice, unreadablePath) || !strings.Contains(notice, corruptedPath) {
		t.Errorf("the notice must name BOTH gap files; got: %q", notice)
	}
	// Stable repoID order: alpha-corrupted before zeta-unreadable.
	if strings.Index(notice, corruptedPath) > strings.Index(notice, unreadablePath) {
		t.Errorf("the notice must list gaps in repoID order (alpha before zeta); got: %q", notice)
	}
	if !strings.Contains(warn.String(), "could not check every project") {
		t.Errorf("the stderr notice must say it could not check every project; got: %q", warn.String())
	}
}
