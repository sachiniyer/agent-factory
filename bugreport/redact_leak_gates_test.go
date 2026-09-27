package bugreport

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/task"
)

// TestLeakReproInHomeResidualLeaf reproduces the in-$HOME residual-leaf leak
// through scrubLog. newRedactor registers the OS username and filepath.Base(home)
// (so they blank to [user]) and collapses $HOME to "~", but a repo deliberately
// kept inside $HOME (but not under a registered repo/worktree root) would still
// ship its private leaf after the "~" prefix collapse, because the typed path
// collapses that whole value to a numbered root the fallback (#4115) does not
// register. The log-only blank must blank the verbatim path so the leaf does not
// survive as "~/confidential-client".
//
// The alternate_path follows the real relocation_recovery shape
// "<repo_path>-<sanitized_title>", so the worktree-title needle recognizes the
// sibling and the repo-prefix blank closes the leaf the "~" collapse strands.
func TestLeakReproInHomeResidualLeaf(t *testing.T) {
	const (
		osUser   = "alice"
		home     = "/home/alice"
		afHome   = home + "/.af"
		repoPath = home + "/confidential-client"
		archive  = afHome + "/archived/0f8fc14cb4d0/fix bug (urgent)"
		altPath  = repoPath + "-fix-bug-urgent"
		title    = "fix bug (urgent)"
	)
	r := &redactor{home: home}
	r.users = appendUserToken(r.users, osUser)
	r.users = appendUserToken(r.users, "alice") // filepath.Base(home)
	r.noteAFHome(afHome)

	fallbackRaw := json.RawMessage(fmt.Sprintf(`[{
		"status": "done",
		"title": %q,
		"worktree": {"repo_path": %q, "worktree_path": %q,
			"relocation_recovery": {"alternate_path": %q}}}]`,
		title, repoPath, archive, altPath))
	r.redactInstancesJSON(fallbackRaw)

	recovery := fmt.Sprintf("archive recovery location: either %s or %s (identity unresolved)", archive, altPath)
	logLine := fmt.Sprintf("WORKTREE_MISSING_DETECTED classification=%q title=%q instance_id=%q repo_path=%q worktree_path=%q recover_error=%q",
		"missing", title, "instance-1", repoPath, archive, recovery)
	got := r.scrubLog(logLine)
	t.Logf("in-$HOME scrubLog out:\n%s", got)
	for _, secret := range []string{osUser, home, "confidential-client", "fix-bug-urgent"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q (residual leak):\n%s", secret, got)
		}
	}
}

// TestLeakReproDefersToRegisteredTaskRoot pins gate 2 of the leaking condition: a
// task whose ProjectPath resolves to the SAME root as the rejected record's
// repo_path registers that root via redactTasks, so the log line collapses to
// the typed-path form [repo:1] rather than blanking to [redacted]. The log-only
// blank must defer to the registered root (registeredRootEquals skips it),
// preserving the layout parity the typed path already gives.
func TestLeakReproDefersToRegisteredTaskRoot(t *testing.T) {
	r := &redactor{}
	r.noteAFHome(siblingLeakAFHome)
	raw := json.RawMessage(fmt.Sprintf(`[{
		"status": "done",
		"title": %q,
		"worktree": {"repo_path": %q, "worktree_path": %q,
			"relocation_recovery": {"alternate_path": %q}}}]`,
		siblingLeakTitle, siblingLeakRepo, siblingLeakArchive, siblingLeakAlternate))
	r.redactInstancesJSON(raw)

	// A task whose ProjectPath IS the rejected record's repo_path. redactTasks
	// registers it as a repo root, closing gate 2 exactly as the typed path would.
	r.redactTasks([]task.Task{{
		ID:            "t1",
		Name:          "nightly",
		CronExpr:      "0 9 * * *",
		TargetSession: siblingLeakTitle,
		ProjectPath:   siblingLeakRepo,
		Program:       "claude",
		Enabled:       true,
	}})

	got := r.scrubLog(siblingRecoveryLogLine())
	t.Logf("registered-root scrubLog out:\n%s", got)
	for _, secret := range []string{siblingLeakRepo, "ConfidentialClient", siblingLeakTitle, siblingLeakDiskTitle} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q after task registered the same root:\n%s", secret, got)
		}
	}
	// The registered root collapses the repo prefix to its numbered token, so the
	// typed-path layout parity holds: [repo:1]-[redacted] rather than [redacted]-[redacted].
	if !strings.Contains(got, "[repo:1]-"+redactedMarker) {
		t.Errorf("scrubLog lost the registered-root layout, want %q in:\n%s", "[repo:1]-"+redactedMarker, got)
	}
}

// TestLeakReproNonSiblingAlternatePath pins the fallback's coverage of an
// alternate_path that is NOT the typed "<repo_path>-<title>" sibling shape.
// worktreeRecoveryLocation interpolates the alternate into recover_error
// verbatim, and the worktree-title needle only recognizes the sibling spelling,
// so a non-sibling (or a malformed record with no usable title) would survive
// the log scrub unless the alternate_path value itself is registered as a
// log-only blank. The bare blank is what closes that leak; the sibling-shape
// defer in appendLogOnlyPathBlankSpans does not apply to a non-needle path.
func TestLeakReproNonSiblingAlternatePath(t *testing.T) {
	const (
		repoPath = "/srv/ConfidentialClient/repo"
		archive  = siblingLeakAFHome + "/archived/0f8fc14cb4d0/fix bug (urgent)"
		title    = siblingLeakTitle
		// alternate_path is a wholly different path, not repoPath + "-<title>".
		altNonSibling = "/srv/Elsewhere/some-other-restore-location"
	)
	r := &redactor{}
	r.noteAFHome(siblingLeakAFHome)
	raw := json.RawMessage(fmt.Sprintf(`[{
		"status": "done",
		"title": %q,
		"worktree": {"repo_path": %q, "worktree_path": %q,
			"relocation_recovery": {"alternate_path": %q}}}]`,
		title, repoPath, archive, altNonSibling))
	r.redactInstancesJSON(raw)

	recovery := fmt.Sprintf("archive recovery location: either %s or %s (identity unresolved)", archive, altNonSibling)
	logLine := fmt.Sprintf("WORKTREE_MISSING_DETECTED classification=%q title=%q instance_id=%q repo_path=%q worktree_path=%q recover_error=%q",
		"missing", title, "instance-1", repoPath, archive, recovery)
	got := r.scrubLog(logLine)
	t.Logf("non-sibling alternate scrubLog out:\n%s", got)
	for _, secret := range []string{altNonSibling, "Elsewhere", "some-other-restore-location", repoPath, "ConfidentialClient"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q (non-sibling alternate_path):\n%s", secret, got)
		}
	}
}

// TestLeakReproWorktreeParentPath pins the fallback's coverage of the daemon
// log's parent_path field. The missing-worktree emitter writes parent_path as
// filepath.Dir of the worktree path (DiagnoseMissingWorktree), so a rejected
// record with worktree_path=/srv/ConfidentialClient/repo-wt puts
// /srv/ConfidentialClient into the log with no registered root and no
// worktree-title needle covering it. Registering the parent spelling when the
// untyped worktree path is collected is what blanks parent_path on the fallback.
func TestLeakReproWorktreeParentPath(t *testing.T) {
	const (
		repoPath     = "/srv/ConfidentialClient/repo"
		worktreePath = "/srv/ConfidentialClient/repo-wt"
		title        = "secret-proj"
	)
	r := &redactor{}
	raw := json.RawMessage(fmt.Sprintf(`[{
		"status": "done",
		"title": %q,
		"worktree": {"repo_path": %q, "worktree_path": %q}}]`,
		title, repoPath, worktreePath))
	r.redactInstancesJSON(raw)

	// The real WORKTREE_MISSING_DETECTED shape: parent_path=filepath.Dir(worktreePath).
	parent := filepath.Dir(worktreePath)
	logLine := fmt.Sprintf("WORKTREE_MISSING_DETECTED classification=%q title=%q instance_id=%q repo_path=%q worktree_path=%q parent_path=%q",
		"missing", title, "instance-1", repoPath, worktreePath, parent)
	got := r.scrubLog(logLine)
	t.Logf("parent_path scrubLog out:\n%s", got)
	for _, secret := range []string{parent, "ConfidentialClient", repoPath, worktreePath, title} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q (worktree parent_path):\n%s", secret, got)
		}
	}
}

// TestLeakReproDescendantOfRegisteredRoot pins the fallback's coverage of a
// rejected record whose repo_path is merely a DESCENDANT of an already
// registered root (here the AF home). The AF home root collapse rewrites only
// the ancestor, so without a log-only blank the private descendant leaf
// (ConfidentialClient/repo) survives the daemon log — the typed path would
// register that exact path as its own root, which the fallback declines per
// #4115. The blank must still fire on the descendant rather than defer to a
// root that only names its ancestor.
func TestLeakReproDescendantOfRegisteredRoot(t *testing.T) {
	const (
		afHome   = "/srv/af"
		repoPath = afHome + "/ConfidentialClient/repo"
	)
	r := &redactor{}
	r.noteAFHome(afHome)
	r.redactInstancesJSON(json.RawMessage(fmt.Sprintf(`[{"status":"legacy","repo_path":%q}]`, repoPath)))

	logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q`, repoPath)
	got := r.scrubLog(logLine)
	t.Logf("descendant-of-root scrubLog out:\n%s", got)
	for _, secret := range []string{repoPath, "ConfidentialClient"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q (descendant of registered root):\n%s", secret, got)
		}
	}
	// The descendant must blank to the marker, not collapse to the root's
	// token-and-remainder form "[af-home]/ConfidentialClient/repo".
	if strings.Contains(got, "[af-home]/ConfidentialClient") {
		t.Errorf("scrubLog left the private descendant leaf under a registered-root ancestor:\n%s", got)
	}
}

// TestLeakReproRetainsRawPathSpelling pins the fallback's coverage of a raw
// path spelling that absolutePathSpellings would clean away. Persisted
// worktrees keep the raw path string verbatim and DiagnoseMissingWorktree logs
// those raw strings, so a double-slash spelling like
// "/srv//ConfidentialClient/repo" must be registered as a log-only blank too —
// the cleaned spelling does not match it and the private path would otherwise
// survive the daemon log scrub.
func TestLeakReproRetainsRawPathSpelling(t *testing.T) {
	const rawRepo = "/srv//ConfidentialClient/repo"
	r := &redactor{}
	r.redactInstancesJSON(json.RawMessage(fmt.Sprintf(`[{"status":"legacy","repo_path":%q}]`, rawRepo)))

	logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q`, rawRepo)
	got := r.scrubLog(logLine)
	t.Logf("raw-spelling scrubLog out:\n%s", got)
	for _, secret := range []string{rawRepo, "/srv/ConfidentialClient/repo", "ConfidentialClient"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q (raw double-slash spelling):\n%s", secret, got)
		}
	}
}

// TestLogOnlyPathBlanksCapBoundsRegistration pins the perf bound the perf review
// of #4938 asked for: the generic fallback registers each rejected record's
// distinct path spellings for log-scope blanking, and appendLogOnlyPathBlankSpans
// scans the daemon-log tail once per registered spelling. A malformed field in a
// large archive falls the whole payload back from the typed decode, so without a
// cap thousands of distinct worktree paths made af bug-report scan a 2 MiB tail
// once per record while handling already-corrupted state. noteLogOnlyPathRedaction
// caps the registry so the scan iterates over a fixed budget rather than the
// rejected-record count, and once the cap is reached further paths are a no-op.
func TestLogOnlyPathBlanksCapBoundsRegistration(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks+100; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("/cap-probe-%d/repo", i))
	}
	// The registry is bounded: a single path admits at most a handful of
	// spellings, so crossing the cap can overshoot by one path's worth, but no
	// further. +8 covers that headroom with margin.
	if len(r.logOnlyPathBlanks) > maxLogOnlyPathBlanks+8 {
		t.Fatalf("logOnlyPathBlanks grew to %d, want <= %d+8 (cap + one path's spellings)",
			len(r.logOnlyPathBlanks), maxLogOnlyPathBlanks)
	}
	// A path registered once the cap is reached is a no-op: it is not a scan
	// needle, which is what holds the per-call scan to the fixed budget.
	const overflow = "/beyond-the-cap/repo"
	r.noteLogOnlyPathRedaction(overflow)
	if r.logOnlyPathBlank(overflow) {
		t.Fatalf("overflow path %q got registered past the cap (registry size %d)",
			overflow, len(r.logOnlyPathBlanks))
	}
}
