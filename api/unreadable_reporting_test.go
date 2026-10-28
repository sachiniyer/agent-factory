package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// TestListSessions_DiskFallbackUnreadableLoud extends #730's loud corrupt-file
// contract to the class it never covered. `sessions list` deliberately refuses
// rather than render a short list, so a caller can tell "no sessions" apart from
// "sessions hidden behind a file I could not open" — and an unreadable file
// hides them exactly as a corrupted one does.
func TestListSessions_DiskFallbackUnreadableLoud(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	stubSnapshot(t, daemonUnavailable)

	seedRows(t, "visible-repo", session.InstanceData{Title: "a", Path: "/repos/visible"})
	seedRows(t, "blocked-repo", session.InstanceData{Title: "b", Path: "/repos/blocked"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	got, err := listSessions("")
	if err == nil {
		t.Fatalf("a partial list must not be rendered as complete (got %d sessions)", len(got))
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error must name the unreadable file %s, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "is a directory") && !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error must carry the underlying I/O error, got: %v", err)
	}
}

// TestDiskWhoami_NamesUnreadableRepoOnNotFound mirrors the corrupted-repo
// caveat this path already appends. "No session for this tmux session" is an
// absence claim, and the file that could not be read is what would refute it.
func TestDiskWhoami_NamesUnreadableRepoOnNotFound(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)

	seedRows(t, "blocked-repo", session.InstanceData{Title: "hidden", TmuxName: "af_x_hidden"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	_, err := diskWhoami("af_x_hidden")
	if err == nil {
		t.Fatalf("whoami must not report a clean not-found while a record is unreadable")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the not-found must name the unreadable file %s, got: %v", path, err)
	}
}

// TestGetSessionByTitle_ReportsIncompleteWideningButStillResolves is the
// report-don't-refuse case, and the boundary against PR 1.
//
// The daemon-side twin of this widening (findSession) REFUSES, because the
// destructive paths resolve through it. This one backstops a read — `sessions
// get` — where breaking a working lookup costs more than the wrong project
// name it prevents, so it resolves and says the check was incomplete. Same rule,
// different consequence.
func TestGetSessionByTitle_ReportsIncompleteWideningButStillResolves(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)

	seedRows(t, "blocked-repo", session.InstanceData{Title: "foo", Path: "/repos/blocked"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	live := []session.InstanceData{{Title: "foo", Path: "/repos/live"}}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

	got, notice, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a read must not break because its ambiguity widening was incomplete: %v", err)
	}
	if got == nil || got.Path != "/repos/live" {
		t.Fatalf("expected the live match to resolve, got: %+v", got)
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

// TestGetSessionByTitleInScope_JSONCarriesIncompleteWideningNotice pins the
// #3511 fix: under --json the ambiguity-widening notice must ride inside the
// envelope's `data` payload — the resolved session plus an additive
// `warnings` array — because stderr under --json carries only the
// {data,error} envelope and drops a free-form line (#3169). Text-output mode
// is unaffected; TestGetSessionByTitle_ReportsIncompleteWideningButStillResolves
// above pins that it still gets the same notice on stderr.
func TestGetSessionByTitleInScope_JSONCarriesIncompleteWideningNotice(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	withEnvelope(t, func() {
		seedRows(t, "blocked-repo", session.InstanceData{Title: "foo", Path: "/repos/blocked"})
		path := makeRecordsUnreadable(t, "blocked-repo")

		live := []session.InstanceData{{Title: "foo", Path: "/repos/live"}}
		stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

		data, notice, err := getSessionByTitleInScope("", "foo")
		if err != nil {
			t.Fatalf("a read must not break because its ambiguity widening was incomplete: %v", err)
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
		if envelope.Data.Path != "/repos/live" {
			t.Fatalf("expected the live match to resolve, got: %+v", envelope.Data)
		}
		if len(envelope.Data.Warnings) != 1 {
			t.Fatalf("expected exactly one warning embedded in data, got: %+v", envelope.Data.Warnings)
		}
		if !strings.Contains(envelope.Data.Warnings[0], path) {
			t.Errorf("the embedded warning must name the unreadable file %s, got: %q", path, envelope.Data.Warnings[0])
		}
		if warn.String() != "" {
			t.Errorf("under --json, stderr must carry nothing — the notice belongs in data, not stderr; stderr was: %q", warn.String())
		}
	})
}

// TestSessionGetPayload_NoNoticeStaysBareData pins the additive half of the
// contract: a clean widening (no notice) must return the bare *InstanceData,
// not a wrapper — an existing --json consumer that decodes only the fields it
// already knows must see byte-identical output to before this fix.
func TestSessionGetPayload_NoNoticeStaysBareData(t *testing.T) {
	data := &session.InstanceData{Title: "foo", Path: "/repos/x"}
	got := sessionGetPayload(data, "")
	if got != any(data) {
		t.Fatalf("expected the bare *InstanceData pointer back when there is no notice, got: %#v", got)
	}
}

// resetIncompleteNotices clears the once-per-invocation dedupe so each test
// starts from a clean slate; the production set is process-lifetime by design.
func resetIncompleteNotices(t *testing.T) {
	t.Helper()
	notedIncompleteAnswers.Range(func(k, _ any) bool {
		notedIncompleteAnswers.Delete(k)
		return true
	})
	t.Cleanup(func() {
		notedIncompleteAnswers.Range(func(k, _ any) bool {
			notedIncompleteAnswers.Delete(k)
			return true
		})
	})
}

// TestIncompleteWideningNoticeIsSaidOnce guards a long-running `sessions watch`.
// An unscoped watch re-resolves the title every poll — roughly 900 times at the
// default 30-minute timeout — and a caveat printed each pass buries whatever
// else the command has to say.
func TestIncompleteWideningNoticeIsSaidOnce(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	seedRows(t, "blocked-repo", session.InstanceData{Title: "foo", Path: "/repos/blocked"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	live := []session.InstanceData{{Title: "foo", Path: "/repos/live"}}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

	for i := 0; i < 25; i++ {
		if _, _, err := getSessionByTitle("foo"); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	// Count the NOTICE, not the path: the path appears twice within one message
	// (once from the skip's own rendering, once inside the wrapped I/O error).
	if got := strings.Count(warn.String(), "warning: could not check every project"); got != 1 {
		t.Fatalf("the caveat must be said once per invocation, not once per poll; said it %d times", got)
	}
	if !strings.Contains(warn.String(), path) {
		t.Errorf("the one notice must still name the unreadable file; stderr was: %q", warn.String())
	}
}

// TestGetSessionByTitle_ReportsEnumerationFailure closes the wider of the two
// holes. When the instances directory itself cannot be read the loader returns
// an error rather than per-repo gaps, and nothing was checked at all — so
// dropping that while reporting the narrower per-repo case would be backwards.
func TestGetSessionByTitle_ReportsEnumerationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	// Make the instances DIRECTORY itself unreadable, so enumeration fails
	// rather than any individual repo. Proven, not assumed.
	seedRows(t, "some-repo", session.InstanceData{Title: "foo", Path: "/repos/x"})
	dir := filepath.Join(home, "instances")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("cannot make the instances directory unreadable in this environment (running as root?)")
	}

	live := []session.InstanceData{{Title: "foo", Path: "/repos/live"}}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

	got, notice, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a read must not break because its widening could not run: %v", err)
	}
	if got == nil || got.Path != "/repos/live" {
		t.Fatalf("expected the live match to resolve, got: %+v", got)
	}
	if !strings.Contains(warn.String(), "could not check any project") {
		t.Errorf("a total enumeration failure must be reported, not silently dropped; stderr was: %q", warn.String())
	}
	if !strings.Contains(notice, "could not check any project") {
		t.Errorf("the returned notice must carry the same enumeration-failure text; got: %q", notice)
	}
}

// TestGetSessionByTitle_ReportsIncompleteWideningOnCorruptedRepo is the
// parse-corruption twin of TestGetSessionByTitle_ReportsIncompleteWideningButStillResolves.
// A repo whose instances.json could not be PARSED is the same kind of gap as one
// whose file could not be READ (#3479): either could hold the title, so the
// widening's exhaustiveness claim must not survive either. Before the fix the
// bare `continue` on json.Unmarshal dropped the corrupted repo from BOTH the
// extra-paths result AND the gaps channel, so the widening reported itself
// exhaustive and a bare `sessions get foo` resolved clean — no notice, no stderr
// line, no log — exactly the wrong-project resolution the widening exists to
// prevent. Now it resolves AND caveats, naming the file so it can be repaired.
func TestGetSessionByTitle_ReportsIncompleteWideningOnCorruptedRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	warnLog := captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	if err := config.SaveRepoInstances("corrupt-repo", json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupt-repo: %v", err)
	}
	path, err := config.RepoInstancesPath("corrupt-repo")
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}

	live := []session.InstanceData{{Title: "foo", Path: "/repos/live"}}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

	got, notice, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a read must not break because its ambiguity widening hit a corrupted file: %v", err)
	}
	if got == nil || got.Path != "/repos/live" {
		t.Fatalf("expected the live match to resolve, got: %+v", got)
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
	// A parse-corrupted instances.json is a file af can READ but not parse, so
	// "remove the file(s)" — RepoInstancesSkipRemedy's advice for an unreadable
	// file — would discard the hidden sessions (af keeps no backup). The notice
	// must carry the corruption-safe hint instead (#5530 review).
	if strings.Contains(notice, "remove the file") {
		t.Errorf("the notice for a corrupted file must not advise removing it (af keeps no backup); got: %q", notice)
	}
	if !strings.Contains(notice, "copy the corrupt") {
		t.Errorf("the notice for a corrupted file must carry the corruption-safe repair hint; got: %q", notice)
	}
	if !strings.Contains(warnLog.String(), "corrupt-repo") {
		t.Errorf("the corrupted repo must be logged while widening; warning log was: %q", warnLog.String())
	}
}

// TestGetSessionByTitle_CorruptedAndUnreadableWideningBothReported pins that the
// two gap classes the widening can hit — a file that could not be READ and one
// whose JSON could not be PARSED — both surface in the one notice, in stable
// repoID order, so an operator gets a single actionable caveat rather than one
// gap silently drowning the other.
func TestGetSessionByTitle_CorruptedAndUnreadableWideningBothReported(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	// zeta-unreadable sorts AFTER alpha-corrupted by repoID, so the order
	// assertion below pins both the "both reported" and the stable-order
	// contracts at once.
	seedRows(t, "zeta-unreadable", session.InstanceData{Title: "foo", Path: "/repos/zeta"})
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

	live := []session.InstanceData{{Title: "foo", Path: "/repos/live"}}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

	got, notice, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("the read must still resolve with both gap classes present: %v", err)
	}
	if got == nil || got.Path != "/repos/live" {
		t.Fatalf("expected the live match to resolve, got: %+v", got)
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

// TestGetSessionByTitleInScope_JSONCarriesCorruptedWideningNotice is the
// parse-corruption twin of TestGetSessionByTitleInScope_JSONCarriesIncompleteWideningNotice:
// under --json the corrupted-widening notice must ride inside the envelope's
// `data` payload (resolved session + additive `warnings` array), because stderr
// under --json carries only the {data,error} envelope and drops a free-form line
// (#3169). Before the fix a corrupted repo produced NO notice at all, so --json
// resolved clean with no warnings array — the silent wrong-project resolution.
func TestGetSessionByTitleInScope_JSONCarriesCorruptedWideningNotice(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	if err := config.SaveRepoInstances("corrupt-repo", json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupt-repo: %v", err)
	}
	path, err := config.RepoInstancesPath("corrupt-repo")
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}

	withEnvelope(t, func() {
		live := []session.InstanceData{{Title: "foo", Path: "/repos/live"}}
		stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

		data, notice, err := getSessionByTitleInScope("", "foo")
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
		if envelope.Data.Path != "/repos/live" {
			t.Fatalf("expected the live match to resolve, got: %+v", envelope.Data)
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
