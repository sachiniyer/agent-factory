package bugreport

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
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

// TestLogOnlyPathBlanksFailClosedPastCap pins the fail-closed half of the
// log-only path cap the #4938 review asked for: once the registry cap is
// reached, noteLogOnlyPathRedaction stops registering further spellings (so the
// per-needle scan stays bounded), but that must not be fail-open — a daemon-log
// tail line for an omitted record would otherwise ship its private path
// verbatim, and the fallback JSON redaction protects a separate section. Once
// the cap is saturated appendLogOnlyPathBlankSpans switches to a single O(text)
// pass that blanks every absolute-path token, so an unregistered past-the-cap
// path still does not survive scrubLog.
func TestLogOnlyPathBlanksFailClosedPastCap(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
	}
	if !r.logOnlyPathBlanksSaturated {
		t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations (cap %d)",
			maxLogOnlyPathBlanks+16, maxLogOnlyPathBlanks)
	}
	// A path registered past the cap is still a no-op for the registry (the
	// bounded scan never sees it) — only the fail-closed pass covers it.
	const overflow = "/past-the-cap-omitted/ConfidentialClient"
	r.noteLogOnlyPathRedaction(overflow)
	if r.logOnlyPathBlank(overflow) {
		t.Fatalf("overflow path %q should not be registered past the cap", overflow)
	}
	// No AF home, no registered roots, no tasks: the overflow path is reachable
	// only by the fail-closed pass, so blanking it isolates that this is what
	// closed the leak.
	logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q worktree_path=%q`,
		overflow, overflow+"/wt")
	got := r.scrubLog(logLine)
	t.Logf("saturated scrubLog out:\n%s", got)
	for _, secret := range []string{overflow, "ConfidentialClient", "past-the-cap-omitted"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q past the cap (fail-open):\n%s", secret, got)
		}
	}
	for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing"} {
		if !strings.Contains(got, want) {
			t.Errorf("fail-closed pass removed non-path triage value %q:\n%s", want, got)
		}
	}
}

// TestWorktreePathTitlesCapBoundsRegistration pins the perf bound the #4938
// review asked for on the sibling worktree-title matcher: rejected records
// sharing one repo_path but carrying thousands of distinct titles used to
// populate an unbounded worktreePathTitles map, and each pair scanned the up
// to 2 MiB daemon-log tail once. The fallback entry point
// (noteFallbackWorktreeTitle) now caps the map so both
// appendWorktreePathTitleSpans and the sibling-prefix loop iterate a fixed
// budget rather than the rejected-record count. The typed entry point
// (noteWorktreeTitle) is separately uncapped — see
// TestWorktreePathTitlesTypedRecordsNotCapped — so the cap falls on the
// fallback explosion only.
func TestWorktreePathTitlesCapBoundsRegistration(t *testing.T) {
	r := &redactor{}
	const repoPath = "/srv/repo"
	for i := 0; i < maxWorktreePathTitles+100; i++ {
		r.noteFallbackWorktreeTitle(repoPath, fmt.Sprintf("title-%d", i))
	}
	// The map is bounded: a single repo_path admits at most a couple of
	// spellings, so crossing the cap can overshoot by one call's worth, but no
	// further.
	if len(r.worktreePathTitles) > maxWorktreePathTitles+2 {
		t.Fatalf("worktreePathTitles grew to %d, want <= %d+2 (cap + one path's spellings)",
			len(r.worktreePathTitles), maxWorktreePathTitles)
	}
	// A title pair registered once the cap is reached is a no-op: it is not a
	// scan needle, which is what holds the per-call scan to the fixed budget.
	before := len(r.worktreePathTitles)
	r.noteFallbackWorktreeTitle(repoPath, "overflow-title-noop")
	if len(r.worktreePathTitles) != before {
		t.Fatalf("overflow fallback title pair got registered past the cap (map size %d -> %d)",
			before, len(r.worktreePathTitles))
	}
}

// TestWorktreePathTitlesTypedRecordsNotCapped pins the #4938 review fix on the
// TYPED path. noteSession calls noteWorktreeTitle for every accepted record,
// so a valid instances.json with more than maxWorktreePathTitles distinct
// titles must still redact every sibling title segment. The cap belongs to the
// fallback entry point (noteFallbackWorktreeTitle); capping the typed path let a
// daemon-log path collapse the registered repo to [repo:n] but ship the
// past-the-cap title segment verbatim beside it.
func TestWorktreePathTitlesTypedRecordsNotCapped(t *testing.T) {
	const repo = "/srv/repo"
	r := &redactor{}
	r.noteRepoRoot(repo)

	// Register many more distinct typed title segments than the fallback cap.
	// The typed path (noteSession) calls noteWorktreeTitle for every accepted
	// record, so a large valid archive must not lose a title segment past the
	// cap the fallback introduced.
	const overflow = maxWorktreePathTitles + 100
	for i := 0; i < overflow; i++ {
		r.noteWorktreeTitle(repo, fmt.Sprintf("Private-title-%d", i))
	}
	if len(r.worktreePathTitles) <= maxWorktreePathTitles {
		t.Fatalf("typed title registration was capped at %d, want > %d (the typed path (noteSession) must not be bounded by the fallback cap)",
			len(r.worktreePathTitles), maxWorktreePathTitles)
	}

	// A past-the-cap typed title's sibling worktree path in the daemon log must
	// still redact the segment and collapse the registered repo to its token.
	pastCapTitle := fmt.Sprintf("Private-title-%d", overflow-1)
	segment := sessiongit.DerivedWorktreePathTitleSegment(repo, pastCapTitle)
	if segment == "" {
		t.Fatalf("DerivedWorktreePathTitleSegment returned empty for %q", pastCapTitle)
	}
	line := repo + "-" + segment + " failed: WORKTREE_MISSING_DETECTED"
	got := r.scrubLog(line)
	if strings.Contains(got, segment) {
		t.Errorf("scrubLog leaked the past-the-cap typed title segment %q:\n%s", segment, got)
	}
	if !strings.Contains(got, "[repo:1]") {
		t.Errorf("scrubLog did not collapse the registered repo root to its token:\n%s", got)
	}
	if strings.Contains(got, repo) {
		t.Errorf("scrubLog leaked the repo path %q:\n%s", repo, got)
	}
	if !strings.Contains(got, "WORKTREE_MISSING_DETECTED") {
		t.Errorf("scrubLog removed non-title triage value:\n%s", got)
	}
}

// TestWorktreePathTitlesFailClosedPastCap pins the fail-closed half of the
// fallback worktree-title cap the #4938 review asked for. Once
// noteFallbackWorktreeTitle reaches maxWorktreePathTitles it stops registering
// further pairs (so the per-needle sibling-prefix and worktree-title scans stay
// bounded), but that must not be fail-open: a daemon-log tail line carrying the
// sibling spelling for an omitted past-the-cap title ("<repo_path>-<title>")
// would otherwise leak both the verbatim repo path and the private title
// segment — the bare-path blank rejects the repo_path because it is
// immediately followed by '-', and the sibling-prefix loop has no registered
// pair to match (knownRootTextBoundary accepts that dash only once the title
// pass has already replaced the suffix with -[redacted], which cannot happen
// for a pair the cap dropped). Once the cap is saturated
// appendLogOnlyPathBlankSpans switches to a single O(text) fail-closed pass
// that blanks every absolute-path token, so an unregistered past-the-cap
// sibling still does not survive scrubLog.
func TestWorktreePathTitlesFailClosedPastCap(t *testing.T) {
	const repoPath = "/srv/ConfidentialClient/repo"
	r := &redactor{}
	// The real fallback (noteUnknownJSONRecord) registers the rejected
	// record's repo_path as a log-only blank alongside the title pairs. The
	// path cap is far from saturated here (one distinct path); only the title
	// cap is, so the leak is isolated to the title-cap fail-closed.
	r.noteLogOnlyPathRedaction(repoPath)
	for i := 0; i < maxWorktreePathTitles+16; i++ {
		r.noteFallbackWorktreeTitle(repoPath, fmt.Sprintf("title-%d", i))
	}
	if !r.worktreePathTitlesSaturated {
		t.Fatalf("worktreePathTitlesSaturated not set after %d fallback registrations (cap %d)",
			maxWorktreePathTitles+16, maxWorktreePathTitles)
	}

	// A past-the-cap fallback title pair was not registered: the per-needle
	// sibling-prefix and worktree-title scans cannot reach it. The fail-closed
	// pass must blank its sibling spelling from the daemon log.
	const pastCapTitle = "Private-overflow-secret"
	segment := sessiongit.DerivedWorktreePathTitleSegment(repoPath, pastCapTitle)
	if segment == "" {
		t.Fatalf("DerivedWorktreePathTitleSegment returned empty for %q", pastCapTitle)
	}
	if _, ok := r.worktreePathTitles[worktreePathTitle{repoPath: repoPath, segment: segment}]; ok {
		t.Fatalf("past-the-cap title pair %q got registered past the cap", pastCapTitle)
	}
	sibling := repoPath + "-" + segment
	logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q recover_error="recovery location: %s"`,
		repoPath, sibling)
	got := r.scrubLog(logLine)
	t.Logf("saturated-title scrubLog out:\n%s", got)
	for _, secret := range []string{repoPath, "ConfidentialClient", segment} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q past the title cap (fail-open):\n%s", secret, got)
		}
	}
	for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing", "recovery location"} {
		if !strings.Contains(got, want) {
			t.Errorf("fail-closed pass removed non-path triage value %q:\n%s", want, got)
		}
	}
}

// TestLogOnlyPathBlanksDuplicateDoesNotSaturate pins the #4938 review fix on
// the duplicate case: noteLogOnlyPathRedaction used to set
// logOnlyPathBlanksSaturated whenever the registry already held
// maxLogOnlyPathBlanks spellings, even if the call was an already-registered
// repo_path that added no new spelling. Rejected records commonly repeat the
// same repo_path, so once the distinct worktree spellings fill the map a
// duplicate would flip the log scrubber to blanking every absolute path and
// drop unrelated diagnostic paths. Saturation must be recorded only when the
// call would actually add a new spelling.
func TestLogOnlyPathBlanksDuplicateDoesNotSaturate(t *testing.T) {
	r := &redactor{}
	// Register distinct paths until the registry first reaches the cap. The
	// call that crosses maxLogOnlyPathBlanks leaves saturation clear (the cap
	// check runs before adding), so the registry is at the cap but not yet
	// saturated.
	var registered string
	for i := 0; ; i++ {
		registered = fmt.Sprintf("/distinct-%d/repo", i)
		r.noteLogOnlyPathRedaction(registered)
		if r.logOnlyPathBlanksSaturated {
			t.Fatalf("registry saturated while registering the distinct path that reached the cap (map %d)",
				len(r.logOnlyPathBlanks))
		}
		if len(r.logOnlyPathBlanks) >= maxLogOnlyPathBlanks {
			break
		}
	}
	if len(r.logOnlyPathBlanks) < maxLogOnlyPathBlanks {
		t.Fatalf("registry did not reach the cap (map %d, want >= %d)",
			len(r.logOnlyPathBlanks), maxLogOnlyPathBlanks)
	}
	// A duplicate of an already-registered repo_path is the realistic case:
	// rejected records commonly repeat the same repo_path. It adds no new
	// needle, so it must not flip the scrubber to blanking every absolute path.
	r.noteLogOnlyPathRedaction(registered)
	if r.logOnlyPathBlanksSaturated {
		t.Fatalf("a duplicate path set logOnlyPathBlanksSaturated even though it added no new spelling (map %d)",
			len(r.logOnlyPathBlanks))
	}
	// A new distinct path past the cap still saturates (fail-closed): the cap
	// must still drop new spellings and switch to the single-pass blank so an
	// omitted record does not ship its private path verbatim.
	r.noteLogOnlyPathRedaction("/beyond-the-cap/new-path")
	if !r.logOnlyPathBlanksSaturated {
		t.Fatalf("a new distinct path past the cap did not saturate (map %d)",
			len(r.logOnlyPathBlanks))
	}
}

// TestLogOnlyPathBlanksFailClosedSaturatedQuotedSpace pins the #4938 review fix
// on the saturated scanner's filename-delimiter handling: Go's %q leaves
// filename-legal bytes such as spaces literal inside a double-quoted value, so
// a daemon-log path field emitted as `repo_path=%q` with a value containing a
// space (e.g. `/srv/Acme Project/SecretRepo`) becomes
// `repo_path="/srv/Acme Project/SecretRepo"`. The fail-closed scanner used to
// stop at the space and blank only the prefix, shipping the private suffix;
// once the path cap is saturated it must blank the whole decoded value.
func TestLogOnlyPathBlanksFailClosedSaturatedQuotedSpace(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
	}
	if !r.logOnlyPathBlanksSaturated {
		t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations (cap %d)",
			maxLogOnlyPathBlanks+16, maxLogOnlyPathBlanks)
	}
	const path = "/srv/Acme Project/SecretRepo"
	logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q`, path)
	got := r.scrubLog(logLine)
	t.Logf("saturated scan with quoted-space path out:\n%s", got)
	for _, secret := range []string{path, "Acme Project", "Project/SecretRepo", "SecretRepo"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q past the saturated scan (space-broken path):\n%s", secret, got)
		}
	}
	for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing"} {
		if !strings.Contains(got, want) {
			t.Errorf("fail-closed pass removed non-path triage value %q:\n%s", want, got)
		}
	}
}

// TestLogOnlyPathBlanksFailClosedSaturatedRecoveryError pins the #4938 review
// fix on the saturated scanner's handling of a prose %q value such as
// recover_error: WORKTREE_MISSING_DETECTED emits recover_error with %q in
// daemon/lostrestore.go, and a recovery message is prose ("recovery location:
// /srv/Acme Project/SecretRepo") rather than a path-valued scalar, so the
// leading-slash whole-value blank does not apply — the path is mid-value (i>0)
// and its first space-broken token ("/srv/Acme") is blanked while the private
// suffix ("Project") survives. The saturated scan must extend the blank through
// the path-with-spaces run so the whole path is redacted, not just its first
// token.
func TestLogOnlyPathBlanksFailClosedSaturatedRecoveryError(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
	}
	if !r.logOnlyPathBlanksSaturated {
		t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations (cap %d)",
			maxLogOnlyPathBlanks+16, maxLogOnlyPathBlanks)
	}
	const path = "/srv/Acme Project/SecretRepo"
	logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" recover_error=%q`,
		"recovery location: "+path)
	got := r.scrubLog(logLine)
	t.Logf("saturated scan with prose recover_error path out:\n%s", got)
	for _, secret := range []string{path, "Acme Project", "Project/SecretRepo", "Project", "SecretRepo"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q past the saturated scan (prose recover_error space-broken path):\n%s", secret, got)
		}
	}
	for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing", "recovery location"} {
		if !strings.Contains(got, want) {
			t.Errorf("fail-closed pass removed non-path triage value %q:\n%s", want, got)
		}
	}
}

// TestWorktreePathTitlesDuplicateDoesNotSaturate pins the #4938 review fix on
// the fallback title duplicate case: noteFallbackWorktreeTitle used to set
// worktreePathTitlesSaturated whenever the map already held maxWorktreePathTitles
// pairs, even if the call was an already-registered (repo_path, title) pair
// that added no matcher. Rejected records commonly repeat the same
// (repo_path, title), and the shared worktreePathTitles map also holds uncapped
// typed-record pairs (noteWorktreeTitle, called by noteSession), so once the
// map filled a duplicate would flip the sibling scrubber to blanking every
// absolute path and drop unrelated diagnostic paths from the entire log.
// Saturation must be recorded only when the fallback call would actually add a
// new pair.
func TestWorktreePathTitlesDuplicateDoesNotSaturate(t *testing.T) {
	r := &redactor{}
	const repoPath = "/srv/repo"
	// Register distinct titles until the map first reaches the cap. The call
	// that crosses maxWorktreePathTitles registers under the cap, so the map
	// is at (or just over) the cap and not yet saturated.
	var registered string
	for i := 0; ; i++ {
		registered = fmt.Sprintf("title-%d", i)
		r.noteFallbackWorktreeTitle(repoPath, registered)
		if r.worktreePathTitlesSaturated {
			t.Fatalf("registry saturated while registering the distinct title that reached the cap (map %d)",
				len(r.worktreePathTitles))
		}
		if len(r.worktreePathTitles) >= maxWorktreePathTitles {
			break
		}
	}
	if len(r.worktreePathTitles) < maxWorktreePathTitles {
		t.Fatalf("registry did not reach the cap (map %d, want >= %d)",
			len(r.worktreePathTitles), maxWorktreePathTitles)
	}
	// A duplicate of an already-registered fallback title is the realistic
	// case: rejected records commonly repeat the same (repo_path, title). It
	// adds no new pair, so it must not flip the sibling scrubber to blanking
	// every absolute path.
	before := len(r.worktreePathTitles)
	r.noteFallbackWorktreeTitle(repoPath, registered)
	if r.worktreePathTitlesSaturated {
		t.Fatalf("a duplicate fallback title set worktreePathTitlesSaturated even though it added no new pair (map %d)",
			len(r.worktreePathTitles))
	}
	if len(r.worktreePathTitles) != before {
		t.Fatalf("a duplicate fallback title changed the map size (map %d -> %d)",
			before, len(r.worktreePathTitles))
	}
	// A new distinct title past the cap still saturates (fail-closed): the
	// cap must still drop new pairs and switch to the single-pass blank so a
	// past-the-cap sibling spelling does not ship the private path verbatim.
	r.noteFallbackWorktreeTitle(repoPath, "beyond-the-cap-new-title")
	if !r.worktreePathTitlesSaturated {
		t.Fatalf("a new distinct fallback title past the cap did not saturate (map %d)",
			len(r.worktreePathTitles))
	}
}

// TestLogOnlyPathBlanksFailClosedSaturatedFilenameLegalDelimiters pins the #4938
// review fix on the saturated scanner's filename-delimiter handling across the
// full set of bytes a path can carry that isPathTextDelimiter also treats as
// terminators. The prior fix covered only a single space; on a Unix filesystem
// only NUL (and the path separator '/', which isPathTextDelimiter deliberately
// does not include) cannot appear inside a name, so a %q-decoded prose value
// such as recover_error="recovery location: /srv/Acme;Project/SecretRepo" used
// to blank only "/srv/Acme" and ship the private "Project/SecretRepo" suffix
// via the saturated scan because the per-token scan stopped at the ';' and the
// extension did not apply. The saturated scan now extends the blank through any
// filename-legal delimiter (space, tab, ';', ',', ':', '=', '\”, '(', ')',
// '[', ']', '{', '}', '<', '>', '&', '|', '`', and repeated spaces) until a
// real terminator (NUL, the closing '"' of a %q field, or the end of the
// decoded value), so the whole path is redacted rather than only its first
// delimiter-free token.
func TestLogOnlyPathBlanksFailClosedSaturatedFilenameLegalDelimiters(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
	}
	if !r.logOnlyPathBlanksSaturated {
		t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations (cap %d)",
			maxLogOnlyPathBlanks+16, maxLogOnlyPathBlanks)
	}
	delimiters := []struct {
		name string
		ins  string
	}{
		{"semicolon", "/srv/Acme;Project/SecretRepo"},
		{"comma", "/srv/Acme,Project/SecretRepo"},
		{"colon", "/srv/Acme:Project/SecretRepo"},
		{"equal", "/srv/Acme=Project/SecretRepo"},
		{"singleQuote", "/srv/Acme'Project/SecretRepo"},
		{"parens", "/srv/Acme(Project)/SecretRepo"},
		{"brackets", "/srv/Acme[Project]/SecretRepo"},
		{"braces", "/srv/Acme{Project}/SecretRepo"},
		{"angle", "/srv/Acme<Project>/SecretRepo"},
		{"amp", "/srv/Acme&Project/SecretRepo"},
		{"pipe", "/srv/Acme|Project/SecretRepo"},
		{"backtick", "/srv/Acme`Project/SecretRepo"},
		{"tab", "/srv/Acme\tProject/SecretRepo"},
		{"repeatedSpace", "/srv/Acme  Project/SecretRepo"},
	}
	for _, tc := range delimiters {
		t.Run(tc.name, func(t *testing.T) {
			logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" recover_error=%q`,
				"recovery location: "+tc.ins)
			got := r.scrubLog(logLine)
			t.Logf("saturated scan with delimiter %q out:\n%s", tc.name, got)
			for _, secret := range []string{tc.ins, "Project", "SecretRepo"} {
				if strings.Contains(got, secret) {
					t.Errorf("scrubLog leaked %q past the saturated scan (filename-legal delimiter %q):\n%s",
						secret, tc.name, got)
				}
			}
			for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing", "recovery location"} {
				if !strings.Contains(got, want) {
					t.Errorf("fail-closed pass removed non-path triage value %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestLogOnlyPathBlanksFailClosedSaturatedDoesNotCrossStructuralQuote pins the
// #4938 review fix on the saturated scanner's '"' boundary: the saturated scan
// also runs on the whole-record view, where a '"' closes a %q value, so
// extending the blank past it would blank unrelated fields from a multi-value
// record. The fail-closed extension excludes '"' specifically (and NUL, which
// cannot appear in a Unix filename), so a recover_error value containing a
// path-with-delimiter blanks the whole path, but the unrelated values that
// follow the closing '"' (the emitter label, classification, repo_path) survive
// as triage signal — except the absolute paths that the fail-closed scan
// blankets alongside the path-with-delimiter, which is the intended fail-
// closed trade the saturated mode already makes.
func TestLogOnlyPathBlanksFailClosedSaturatedDoesNotCrossStructuralQuote(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
	}
	if !r.logOnlyPathBlanksSaturated {
		t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations (cap %d)",
			maxLogOnlyPathBlanks+16, maxLogOnlyPathBlanks)
	}
	logLine := `WORKTREE_MISSING_DETECTED classification="missing" recover_error="recovery location: /srv/Acme;SecretRepo" branch_name="fix-proj"`
	got := r.scrubLog(logLine)
	t.Logf("saturated scan whole-record out:\n%s", got)
	for _, secret := range []string{"/srv/Acme", "Acme", "SecretRepo"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q across the saturated scan:\n%s", secret, got)
		}
	}
	// A quoted structural value after the saturated recover_error survives as
	// triage value — this is the case the '"' boundary exists to protect.
	if !strings.Contains(got, "fix-proj") {
		t.Errorf("saturated scan crossed a structural quote and blanked an unrelated triage value:\n%s", got)
	}
	if !strings.Contains(got, "WORKTREE_MISSING_DETECTED") {
		t.Errorf("saturated scan removed the emitter label:\n%s", got)
	}
}

// TestLogOnlyPathBlanksRelativeRejectedRecord pins the #4938 review fix on the
// generic fallback's handling of a RELATIVE repo_path or worktree_path in a
// rejected record. NewGitWorktreeFromStorage only rejects empty paths, so a
// hand-edited or legacy instances.json can carry a relative value such as
// "private-client/repo"; absolutePathSpellings returns nil for it, so the
// registration loop used to discard it and logVanishedWorktreeOnce then wrote
// the stored value with %q, shipping the verbatim relative path in a tail
// line such as repo_path="private-client/repo" even though the fallback JSON
// redacts that field wholesale. noteLogOnlyPathRedaction now registers the
// cleaned relative spelling (when the path carries a path separator, so a
// single bare word is not blanked from prose) alongside the absolute spellings
// it already kept, so the bare-blank scan reaches it the same way an absolute
// path is reached.
func TestLogOnlyPathBlanksRelativeRejectedRecord(t *testing.T) {
	for _, tc := range []struct {
		name     string
		repoPath string
		worktree string
	}{
		{"multi-segment", "private-client/repo", "private-client/repo-wt"},
		{"raw-dot-slash", "./private-client/repo", "./private-client/repo-wt"},
		{"double-dot-parent", "../client/repo", "../client/repo-wt"},
		{"three-segment", "a/b/repo", "a/b/repo-wt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &redactor{}
			r.redactInstancesJSON(json.RawMessage(fmt.Sprintf(
				`[{"status":"legacy","repo_path":%q,"worktree_path":%q}]`,
				tc.repoPath, tc.worktree)))
			logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q worktree_path=%q`,
				tc.repoPath, tc.worktree)
			got := r.scrubLog(logLine)
			t.Logf("relative rejected-record scrubLog out:\n%s", got)
			for _, secret := range []string{tc.repoPath, tc.worktree} {
				if strings.Contains(got, secret) {
					t.Errorf("scrubLog leaked relative path %q:\n%s", secret, got)
				}
			}
			for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing"} {
				if !strings.Contains(got, want) {
					t.Errorf("scrubLog removed non-path triage value %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestLogOnlyPathBlanksRelativeRejectedRecordSaturated pins the fail-closed
// half of the relative-path #4938 review fix. Once the path cap saturates the
// per-needle scan is replaced by the fail-closed scan, which must also reach a
// relative path embedded mid-value: a relative path's '/' is interior, so the
// scanner walks backward from that '/' through the previous path-legal bytes
// to the previous delimiter (or the start of the view) and blanks from there.
// For an absolute path the walk is a no-op (the byte before '/' is already a
// delimiter), so the existing absolute-path fail-closed behavior is unchanged.
func TestLogOnlyPathBlanksRelativeRejectedRecordSaturated(t *testing.T) {
	for _, tc := range []struct {
		name     string
		repoPath string
		worktree string
	}{
		{"multi-segment", "private-client/repo", "private-client/repo-wt"},
		{"raw-dot-slash", "./private-client/repo", "./private-client/repo-wt"},
		{"double-dot-parent", "../client/repo", "../client/repo-wt"},
		{"three-segment", "a/b/repo", "a/b/repo-wt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &redactor{}
			for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
				r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
			}
			if !r.logOnlyPathBlanksSaturated {
				t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations", maxLogOnlyPathBlanks+16)
			}
			r.redactInstancesJSON(json.RawMessage(fmt.Sprintf(
				`[{"status":"legacy","repo_path":%q,"worktree_path":%q}]`,
				tc.repoPath, tc.worktree)))
			logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q worktree_path=%q`,
				tc.repoPath, tc.worktree)
			got := r.scrubLog(logLine)
			t.Logf("saturated relative scrubLog out:\n%s", got)
			for _, secret := range []string{tc.repoPath, tc.worktree} {
				if strings.Contains(got, secret) {
					t.Errorf("scrubLog leaked relative path %q past the saturated scan:\n%s", secret, got)
				}
			}
			for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing"} {
				if !strings.Contains(got, want) {
					t.Errorf("fail-closed pass removed non-path triage value %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestWorktreePathTitlesTypedEntriesDoNotConsumeFallbackCap pins the #4938
// review fix on the typed/fallback cap interaction. The shared
// worktreePathTitles map holds both uncapped typed-record pairs
// (noteWorktreeTitle, called by noteSession) and capped fallback pairs
// (noteFallbackWorktreeTitle). Counting the typed pairs toward the fallback
// cap meant a valid large archive that already filled the map with typed
// titles saturated on a later repository's first rejected record — one new
// fallback pair would flip the sibling scrubber to blanking every path and
// drop unrelated diagnostic paths from the entire log even though the
// fallback registry itself never approached the fallback cap. The cap now
// counts only the fallback pairs added (worktreePathTitlesFallback), so the
// typed archive's per-title layout survives and a later rejected record's
// first fallback pair registers cleanly instead of saturating.
func TestWorktreePathTitlesTypedEntriesDoNotConsumeFallbackCap(t *testing.T) {
	const repo = "/srv/repo"
	r := &redactor{}
	r.noteRepoRoot(repo)
	// A valid large archive fills the map with TYPED titles. The typed path
	// (noteWorktreeTitle, called by noteSession) is uncapped, so the typed
	// pairs must NOT count toward the fallback cap; the prior behavior
	// saturated here, before any fallback call had a chance to register.
	for i := 0; i < maxWorktreePathTitles+100; i++ {
		r.noteWorktreeTitle(repo, fmt.Sprintf("Typed-title-%d", i))
	}
	if r.worktreePathTitlesSaturated {
		t.Fatalf("typed pre-population saturated the fallback cap (map %d, fallback count %d)",
			len(r.worktreePathTitles), r.worktreePathTitlesFallback)
	}
	// A later repository with one rejected record arrives. Its FIRST new
	// fallback pair must not saturate: the fallback registry is still far
	// below the fallback cap, so blanking every path from the daemon tail
	// would drop unrelated diagnostic paths from an otherwise valid report.
	// The shared map holding thousands of typed entries is not a fallback
	// saturation.
	const fallbackTitle = "fallback-secret"
	r.noteFallbackWorktreeTitle(repo, fallbackTitle)
	if r.worktreePathTitlesSaturated {
		t.Fatalf("the first fallback pair saturated because the cap counted typed entries (map %d, fallback count %d)",
			len(r.worktreePathTitles), r.worktreePathTitlesFallback)
	}
	// The fallback pair IS registered: the per-needle sibling scan can still
	// reach it, the same way the typed path reaches a past-the-typed-cap
	// title.
	segment := sessiongit.DerivedWorktreePathTitleSegment(repo, fallbackTitle)
	if segment == "" {
		t.Fatalf("DerivedWorktreePathTitleSegment returned empty for %q", fallbackTitle)
	}
	if _, ok := r.worktreePathTitles[worktreePathTitle{repoPath: repo, segment: segment}]; !ok {
		t.Fatalf("the first fallback pair was not registered (cap %d, map %d, fallback count %d)",
			maxWorktreePathTitles, len(r.worktreePathTitles), r.worktreePathTitlesFallback)
	}
	if r.worktreePathTitlesFallback != 1 {
		t.Fatalf("fallback counter = %d, want 1 after registering one new fallback pair",
			r.worktreePathTitlesFallback)
	}
}

// TestLogOnlyPathBlanksSingleSegmentRelativeRejectedRecord pins the #4938
// review fix on the generic fallback's handling of a SINGLE-SEGMENT relative
// path in a rejected record. NewGitWorktreeFromStorage only rejects empty
// paths, so a hand-edited or legacy instances.json can carry a bare directory
// name — either directly as a single-segment repo_path="ConfidentialClient",
// or as the parent_path the missing-worktree emitter writes for a multi-
// segment worktree_path="ConfidentialClient/wt" (parent_path=filepath.Dir).
// relativePathSpellings used to discard the single-segment name to avoid
// over-blanking prose for a private directory whose name is one bare word,
// shipping the verbatim private directory name through the daemon-log
// section even though the fallback JSON redacts that field wholesale. The
// #4938 review accepted the over-blank trade-off found; the bare directory
// name now registers for log-scope blanking in a separate bounded set
// (logOnlyPathBareNames) whose per-needle scan reaches it.
func TestLogOnlyPathBlanksSingleSegmentRelativeRejectedRecord(t *testing.T) {
	for _, tc := range []struct {
		name     string
		repoPath string
		worktree string
		secret   string
		logKey   string
	}{
		{
			name:     "single-segment repo_path",
			repoPath: "ConfidentialClient",
			worktree: "ConfidentialClient/wt",
			secret:   "ConfidentialClient",
			logKey:   "repo_path",
		},
		{
			name:     "single-segment parent_path",
			repoPath: "key/repo",
			worktree: "ConfidentialClient/wt",
			secret:   "ConfidentialClient",
			logKey:   "parent_path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &redactor{}
			r.redactInstancesJSON(json.RawMessage(fmt.Sprintf(
				`[{"status":"legacy","repo_path":%q,"worktree_path":%q}]`,
				tc.repoPath, tc.worktree)))
			if _, ok := r.logOnlyPathBareNames[tc.secret]; !ok {
				t.Fatalf("logOnlyPathBareNames did not register the single-segment name %q (set %v)",
					tc.secret, r.logOnlyPathBareNames)
			}
			logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q worktree_path=%q parent_path=%q`,
				tc.repoPath, tc.worktree, filepath.Dir(tc.worktree))
			got := r.scrubLog(logLine)
			t.Logf("single-segment relative scrubLog out:\n%s", got)
			if strings.Contains(got, tc.secret) {
				t.Errorf("scrubLog leaked single-segment relative %q:\n%s", tc.secret, got)
			}
			for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing"} {
				if !strings.Contains(got, want) {
					t.Errorf("fail-closed pass removed non-path triage value %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestLogOnlyPathBlanksSingleSegmentRelativeRejectedRecordSaturated pins the
// saturated half of the #4938 review fix for a single-segment relative path.
// Once the slash-bearing path cap saturates the per-needle scan against
// logOnlyPathBlanks is replaced by the saturated scan, which anchors on '/'
// and cannot reach a single-segment name with no separator. The bare-name
// set is independent of the slash-bearing cap, so the per-needle pass
// against logOnlyPathBareNames still reaches the bare name even after the
// slash-bearing registry has saturated (#4938 review).
func TestLogOnlyPathBlanksSingleSegmentRelativeRejectedRecordSaturated(t *testing.T) {
	const secret = "ConfidentialClient"
	for _, tc := range []struct {
		name     string
		repoPath string
		worktree string
	}{
		{"single-segment repo_path", "ConfidentialClient", "ConfidentialClient/wt"},
		{"single-segment parent_path", "key/repo", "ConfidentialClient/wt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &redactor{}
			for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
				r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
			}
			if !r.logOnlyPathBlanksSaturated {
				t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations", maxLogOnlyPathBlanks+16)
			}
			r.redactInstancesJSON(json.RawMessage(fmt.Sprintf(
				`[{"status":"legacy","repo_path":%q,"worktree_path":%q}]`,
				tc.repoPath, tc.worktree)))
			if _, ok := r.logOnlyPathBareNames[secret]; !ok {
				t.Fatalf("logOnlyPathBareNames did not register the bare name %q after the slash-bearing cap saturated (set %v)",
					secret, r.logOnlyPathBareNames)
			}
			logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q worktree_path=%q parent_path=%q`,
				tc.repoPath, tc.worktree, filepath.Dir(tc.worktree))
			got := r.scrubLog(logLine)
			t.Logf("saturated single-segment relative scrubLog out:\n%s", got)
			if strings.Contains(got, secret) {
				t.Errorf("scrubLog leaked single-segment relative %q past the saturated scan:\n%s", secret, got)
			}
			for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing"} {
				if !strings.Contains(got, want) {
					t.Errorf("fail-closed pass removed non-path triage value %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestLogOnlyPathBlanksRelativeSaturatedDelimiterBeforeFirstSlash pins the
// #4938 review fix on the saturated scanner's backward walk for a relative
// path whose first segment carries a filename-legal delimiter before the
// first slash (e.g. "private:client/repo" or "private client/repo"). The
// prior walk-back stopped at ':' or the space and blanked only the suffix
// "client/repo", shipping the private "private" prefix in the %q-decoded
// log value even though the saturated scan is supposed to be fail-closed.
// A delimiter flanked by path-legal bytes is interior to a relative path's
// first segment, so the saturated scan now extends the walk-back through
// it; a delimiter next to a structural separator (the ':' in
// "location: /srv") is the prose-to-path boundary, so prose like
// "recovery location:" survives while the embedded relative path blanks
// whole (#4938 review).
func TestLogOnlyPathBlanksRelativeSaturatedDelimiterBeforeFirstSlash(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"colon", "private:client/repo"},
		{"space", "private client/repo"},
		{"semicolon", "private;client/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &redactor{}
			for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
				r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
			}
			if !r.logOnlyPathBlanksSaturated {
				t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations (cap %d)",
					maxLogOnlyPathBlanks+16, maxLogOnlyPathBlanks)
			}
			r.redactInstancesJSON(json.RawMessage(fmt.Sprintf(
				`[{"status":"legacy","repo_path":%q,"worktree_path":%q}]`,
				tc.path, tc.path+"-wt")))
			logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q worktree_path=%q`,
				tc.path, tc.path+"-wt")
			got := r.scrubLog(logLine)
			t.Logf("saturated relative with %q delimiter scrubLog out:\n%s", tc.name, got)
			for _, secret := range []string{tc.path, "private", "client", tc.path + "-wt"} {
				if strings.Contains(got, secret) {
					t.Errorf("scrubLog leaked %q past the saturated scan (delimiter %q):\n%s",
						secret, tc.name, got)
				}
			}
			for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing"} {
				if !strings.Contains(got, want) {
					t.Errorf("fail-closed pass removed non-path triage value %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestLogOnlyPathBlanksFailClosedSaturatedQuotedFilenameChar pins the #4938 review
// fix on the saturated scanner's handling of a literal '"' inside a path. A
// Unix filename may contain '"', and goQuoteTransform recurses over every %q
// field as ProvLogValue, where the decoded view carries the '"' as filename
// data rather than as the structural terminator of a %q value. The saturated
// scan used to refuse the continuation across '"' in every view, so a prose
// recover_error such as `recovery location: /srv/Acme"SecretRepo` blanked only
// "/srv/Acme" and shipped the private '"SecretRepo' suffix in the bundled log.
// On the decoded ProvLogValue view the scan now extends the blank through the
// '"' (it is content); on the whole-record view it still stops at '"' (it
// closes the %q field), which TestLogOnlyPathBlanksFailClosedSaturatedDoesNotCrossStructuralQuote
// pins.
func TestLogOnlyPathBlanksFailClosedSaturatedQuotedFilenameChar(t *testing.T) {
	r := &redactor{}
	for i := 0; i < maxLogOnlyPathBlanks+16; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("/under-cap-%d/repo", i))
	}
	if !r.logOnlyPathBlanksSaturated {
		t.Fatalf("logOnlyPathBlanksSaturated not set after %d registrations (cap %d)",
			maxLogOnlyPathBlanks+16, maxLogOnlyPathBlanks)
	}
	const path = `/srv/Acme"SecretRepo`
	logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" recover_error=%q`,
		"recovery location: "+path)
	got := r.scrubLog(logLine)
	t.Logf("saturated scan with quoted-filename-char path out:\n%s", got)
	for _, secret := range []string{path, `Acme"SecretRepo`, `"SecretRepo`, "SecretRepo"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q past the saturated scan (quoted filename char):\n%s", secret, got)
		}
	}
	for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing", "recovery location"} {
		if !strings.Contains(got, want) {
			t.Errorf("fail-closed pass removed non-path triage value %q:\n%s", want, got)
		}
	}
}

// TestLogOnlyPathBareNamesSaturatedFailsClosed pins the #4938 review fix on the
// bare-name cap's overflow. noteLogOnlyPathRedaction's bare-name loop used to
// break once logOnlyPathBareNames reached maxLogOnlyPathBlanks, silently
// dropping every later single-segment relative name with no saturation flag.
// The slash-bearing saturated scan anchors on '/' and cannot reach a bare
// name, and the per-needle pass has no entry for the dropped name, so a
// daemon-tail record for a past-the-cap bare name such as
// repo_path="ConfidentialClient4097" shipped the private name verbatim.
// noteLogOnlyPathRedaction now sets logOnlyPathBareNamesSaturated at the cap,
// and the matcher fail-closed-blanks the dropped name: on the decoded %q
// scalar it blanks the whole no-'/' scalar (repo_path here), and on the whole
// daemon-log record appendSaturatedBareNameSpans blanks every unquoted
// bare-name-shaped token, so the dropped name does not survive the daemon log
// even when the record also carries a '/' (a branch ref beside an unquoted
// workDir, the session/backend_local_respawn.go "at %s" shape).
//
// A '/'-bearing scalar is a real path and stays under the slash-bearing scan
// (which is NOT saturated here), so the unrelated absolute path in
// recover_error survives: the per-scalar pass leaves it alone and the
// whole-record scan leaves the quoted scalar to that per-scalar pass. The
// unquoted emitter label and field names on the same record ARE bare-name-shaped
// tokens, so the whole-record fail-closed blanks them too — the privacy side
// of the same fail-closed trade the slash-bearing saturated scan already makes
// for every '/'-bearing token in the degenerate archive that saturates the
// bare-name set (#4938 review).
func TestLogOnlyPathBareNamesSaturatedFailsClosed(t *testing.T) {
	r := &redactor{}
	// Register maxLogOnlyPathBlanks distinct single-segment bare names (the cap
	// before saturation).
	for i := 0; i < maxLogOnlyPathBlanks; i++ {
		r.noteLogOnlyPathRedaction(fmt.Sprintf("bare-name-%d", i))
	}
	if r.logOnlyPathBareNamesSaturated {
		t.Fatalf("logOnlyPathBareNamesSaturated set before the cap was reached (cap %d)", maxLogOnlyPathBlanks)
	}
	if len(r.logOnlyPathBareNames) != maxLogOnlyPathBlanks {
		t.Fatalf("bare-name set did not reach the cap (got %d, want %d)",
			len(r.logOnlyPathBareNames), maxLogOnlyPathBlanks)
	}
	// A new distinct bare name past the cap saturates (fail-closed) and is not
	// registered: dropping it silently is the leak this fix closes.
	const secret = "ConfidentialClient4097"
	r.noteLogOnlyPathRedaction(secret)
	if !r.logOnlyPathBareNamesSaturated {
		t.Fatalf("logOnlyPathBareNamesSaturated not set after a new bare name past the cap (cap %d)", maxLogOnlyPathBlanks)
	}
	if _, ok := r.logOnlyPathBareNames[secret]; ok {
		t.Fatalf("past-the-cap bare name %q was registered into the capped set (set size %d)",
			secret, len(r.logOnlyPathBareNames))
	}
	// A duplicate bare name at the cap adds no new needle, so it must not grow
	// the set or change the saturation flag (#4938 review's duplicate guard).
	dupBefore := len(r.logOnlyPathBareNames)
	r.noteLogOnlyPathRedaction("bare-name-0")
	if len(r.logOnlyPathBareNames) != dupBefore {
		t.Fatalf("a duplicate bare name changed the set size (%d -> %d)", dupBefore, len(r.logOnlyPathBareNames))
	}
	// A daemon-tail line for the omitted bare name. repo_path is a bare scalar
	// (no '/') so the per-scalar bare-name fail-closed blanks it whole; the
	// whole-record scan blanks the unquoted emitter label and field names
	// (bare-name-shaped tokens) but leaves the quoted recover_error scalar to
	// the per-scalar pass, which — recover_error carrying a '/' and the
	// slash-bearing registry empty and not saturated — leaves the unrelated
	// path verbatim. So the secret is gone, the '/'-bearing quoted path
	// survives, and only the unquoted bare tokens of the record blank.
	logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q recover_error=%q`,
		secret, "recovery location: /srv/unrelated/repo")
	got := r.scrubLog(logLine)
	t.Logf("bare-name-saturated scrubLog out:\n%s", got)
	if strings.Contains(got, secret) {
		t.Errorf("scrubLog leaked past-the-cap bare name %q:\n%s", secret, got)
	}
	for _, want := range []string{"/srv/unrelated/repo", "recovery location"} {
		if !strings.Contains(got, want) {
			t.Errorf("scrubLog removed %q that the '/'-bearing quoted scalar must keep (#4938 review):\n%s", want, got)
		}
	}
	if strings.Contains(got, "WORKTREE_MISSING_DETECTED") {
		t.Errorf("scrubLog left the unquoted emitter label %q blankable by the whole-record bare-name fail-closed:\n%s",
			"WORKTREE_MISSING_DETECTED", got)
	}
}

// TestLogOnlyPathBareNamesSaturatedFailsClosedUnquotedRecord pins the #4938
// review fix on the bare-name cap's overflow for a bare path that the daemon
// log emits UNQUOTED. The earlier fail-closed blanked only a decoded %q scalar
// (ProvLogValue), so a past-the-cap bare name logged via %s —
// session/backend_local_respawn.go:129 and :132 log workDir with %s, so a tail
// line such as "recover: rebuilt missing worktree for session %q at %s from
// branch main" carries the bare name verbatim in the whole daemon-log record
// (quoteStructural), where the per-needle pass has no entry for the dropped
// name and the slash-bearing saturated scan cannot anchor on a '/' the bare
// name does not have. The fail-closed now blanks the whole view when it
// carries no '/' — the shape of a bare-name-only record — so the unquoted bare
// name does not survive the daemon log either, while a '/'-bearing record is
// still left to the slash-bearing scan and the per-needle pass (#4938 review).
func TestLogOnlyPathBareNamesSaturatedFailsClosedUnquotedRecord(t *testing.T) {
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
	if _, ok := r.logOnlyPathBareNames[secret]; ok {
		t.Fatalf("past-the-cap bare name %q was registered into the capped set (set size %d)",
			secret, len(r.logOnlyPathBareNames))
	}
	// The bare name is logged via %s (NOT %q), so it stays in the whole
	// daemon-log record rather than entering as a decoded %q scalar, and the
	// record carries no '/' at all — the shape a bare-name-only record has.
	logLine := fmt.Sprintf(`recover: rebuilt missing worktree for session %q at %s from branch main`,
		"fix-bug-urgent", secret)
	got := r.scrubLog(logLine)
	t.Logf("unquoted bare-name-saturated scrubLog out:\n%s", got)
	if strings.Contains(got, secret) {
		t.Errorf("scrubLog leaked past-the-cap unquoted bare name %q on the whole-record view:\n%s", secret, got)
	}
}

// TestLogOnlyPathBlankRelativeRawSpellingCleansToDot pins the #4938 review fix
// on a relative path whose filepath.Clean collapses to "." or "..".
// relativePathSpellings used to return nil for such a path, discarding the
// raw spelling along with the cleaned trivial alias, so a rejected record
// carrying repo_path="ConfidentialClient/.." registered nothing: the cleaned
// "." names nothing private, but logVanishedWorktreeOnce emits the raw value
// via %q, so a daemon-tail line repo_path="ConfidentialClient/.." shipped the
// private directory name verbatim even though the fallback JSON redacts that
// field wholesale. relativePathSpellings now keeps the raw spelling (it
// carries a separator, so it routes to the slash-bearing log-only blank) and
// skips only the cleaned trivial alias, so the verbatim raw value does not
// survive the daemon log (#4938 review).
func TestLogOnlyPathBlankRelativeRawSpellingCleansToDot(t *testing.T) {
	r := &redactor{}
	const repoPath = "ConfidentialClient/.."
	r.redactInstancesJSON(json.RawMessage(fmt.Sprintf(
		`[{"status":"legacy","repo_path":%q}]`,
		repoPath)))
	logLine := fmt.Sprintf(`WORKTREE_MISSING_DETECTED classification="missing" repo_path=%q`, repoPath)
	got := r.scrubLog(logLine)
	t.Logf("clean-to-dot raw relative scrubLog out:\n%s", got)
	for _, secret := range []string{repoPath, "ConfidentialClient"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked clean-to-dot raw relative spelling %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{"WORKTREE_MISSING_DETECTED", "missing"} {
		if !strings.Contains(got, want) {
			t.Errorf("scrubLog removed non-path triage value %q:\n%s", want, got)
		}
	}
}

// TestFallbackWorktreeTitleRegistersRelativeRepoPath pins the #4938 review fix
// on the sibling-title pair the fallback registers for a RELATIVE repo_path.
// noteFallbackWorktreeTitle used to iterate only absolutePathSpellings, which
// returns nil for a relative path, so a rejected record carrying a relative
// repo_path registered no (repo_path, title) pair. A daemon recovery path such
// as "<repo_path>-<segment>" (e.g. "private/client-fix-bug-urgent" for title
// "fix bug (urgent)") then survived the sibling redaction verbatim: the bare
// relative spelling IS a log-only blank, but it is immediately followed by '-',
// so knownRootTextBoundary rejects the occurrence (a sibling dash is not a text
// delimiter), and with no registered pair the sibling-prefix loop has nothing to
// match. noteFallbackWorktreeTitle now iterates the relative spellings too, so
// the sibling needle is built on the same spelling the daemon log carries and
// the sibling-prefix blank reaches it (#4938 review).
//
// Fail-first: without the relative spellings the sibling-prefix loop has no
// registered pair, the bare blank rejects "private/client" before the '-', and
// "private/client" ships verbatim beside the segment.
func TestFallbackWorktreeTitleRegistersRelativeRepoPath(t *testing.T) {
	r := &redactor{}
	const (
		repoPath = "private/client"
		title    = "fix bug (urgent)"
	)
	// noteUnknownJSONRecord registers the relative repo_path as a slash-bearing
	// log-only blank (so the sibling-prefix loop's logOnlyPathBlank gate opens)
	// AND as a fallback sibling pair. Drive both exactly as the fallback does.
	r.noteLogOnlyPathRedaction(repoPath)
	r.noteFallbackWorktreeTitle(repoPath, title)

	segment := sessiongit.DerivedWorktreePathTitleSegment(repoPath, title)
	if segment == "" {
		t.Fatalf("DerivedWorktreePathTitleSegment returned empty for repoPath=%q title=%q", repoPath, title)
	}
	needle := repoPath + "-" + segment
	if !r.isWorktreeTitleSiblingNeedle(needle) {
		t.Fatalf("relative sibling needle %q is not registered (worktreeTitleSiblingNeedles=%v)",
			needle, r.worktreeTitleSiblingNeedles)
	}

	// A daemon recovery line carrying the sibling spelling in the open
	// (unquoted) record. The bare blank rejects the relative repo_path before
	// the sibling dash, so only the registered pair redacts it.
	logLine := "recover: rebuilt missing worktree for session %q at %s from branch main"
	got := r.scrubLog(fmt.Sprintf(logLine, "other-session", needle))
	t.Logf("relative sibling scrubLog out:\n%s", got)
	for _, secret := range []string{repoPath, segment} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked the relative sibling %q:\n%s", secret, got)
		}
	}
}

// TestLogOnlyPathBareNamesSaturatedFailsClosedUnquotedRecordWithSlash pins the
// #4938 review fix on the bare-name cap's overflow when the daemon-log record
// that carries the past-the-cap bare name ALSO carries a '/'. The earlier
// fail-closed blanked the whole record only when it carried no '/', so a bare
// name logged unquoted via %s — session/backend_local_respawn.go:129 and :132
// print workDir beside "branch %s" — leaked verbatim once the common branch
// value contained a '/' (feature/foo): the '/' gate suppressed the whole-record
// fallback, the per-needle pass had no entry for the dropped name, and the
// slash-bearing saturated scan (not saturated here) cannot anchor on a '/' a
// bare name does not have. appendSaturatedBareNameSpans now walks the record
// and blanks every unquoted bare-name-shaped token, so the workDir is redacted
// in a '/'-bearing record too. The branch's '/' survives, so the slash-bearing
// shape is not destroyed, and the over-blank is the privacy side of the same
// fail-closed trade the slash-bearing saturated scan already makes (#4938
// review).
//
// Fail-first: with the no-'/' whole-record blank, the '/' in "feature/foo"
// suppresses the fallback and "ConfidentialClient4097" ships verbatim.
func TestLogOnlyPathBareNamesSaturatedFailsClosedUnquotedRecordWithSlash(t *testing.T) {
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
	if _, ok := r.logOnlyPathBareNames[secret]; ok {
		t.Fatalf("past-the-cap bare name %q was registered into the capped set (set size %d)",
			secret, len(r.logOnlyPathBareNames))
	}
	// The bare name is logged via %s (NOT %q), so it stays in the whole
	// daemon-log record rather than entering as a decoded %q scalar, and the
	// SAME record carries a '/' from the common branch value feature/foo.
	logLine := fmt.Sprintf(`recover: rebuilt missing worktree for session %q at %s from recorded base and recreated branch %s`,
		"fix-bug-urgent", secret, "feature/foo")
	got := r.scrubLog(logLine)
	t.Logf("unquoted + slash-bearing bare-name-saturated scrubLog out:\n%s", got)
	if strings.Contains(got, secret) {
		t.Errorf("scrubLog leaked past-the-cap unquoted bare name %q on a '/'-bearing whole-record view:\n%s", secret, got)
	}
	// The branch's '/' survives the bare-name fail-closed (the scan splits at
	// the separator; only the segment that starts at a text boundary blanks),
	// so the slash-bearing shape of the record is not destroyed.
	if !strings.Contains(got, "/") {
		t.Errorf("scrubLog lost every '/' of the '/'-bearing record, the bare-name scan must preserve slash-bearing shape:\n%s", got)
	}
}

// TestWorktreeTitleSiblingNeedleIndexed pins the #4938 review fix on
// isWorktreeTitleSiblingNeedle. The membership test runs once per registered
// path in appendLogOnlyPathBlankSpans and appendBareNameLogOnlyPathBlankSpans,
// and the linear scan it used to make built and compared the full needle for
// every worktree-path-title pair on every call — a ~16M comparison cross-product
// when both fallback registries sit near their 4096-entry caps. The needles are
// now indexed in worktreeTitleSiblingNeedles alongside the pairs, so the test is
// a single map lookup, and the set mirrors worktreePathTitles exactly: every
// pair the per-needle loops iterate is indexed, and a pair dropped at the
// fallback cap is added to neither (#4938 review).
func TestWorktreeTitleSiblingNeedleIndexed(t *testing.T) {
	r := &redactor{}
	const (
		typedRepo  = "/srv/typed-repo"
		fbRepoPath = "private/client"
	)
	typedTitle := "Typed title one"
	fbTitle := "fix bug (urgent)"

	// A typed pair (noteWorktreeTitle) and a fallback pair
	// (noteFallbackWorktreeTitle) each index their needle. The fallback pair
	// also needs the repo_path registered as a log-only blank for the
	// sibling-prefix loop, mirroring noteUnknownJSONRecord; that is not what
	// this test asserts, only the needle index.
	r.noteWorktreeTitle(typedRepo, typedTitle)
	r.noteLogOnlyPathRedaction(fbRepoPath)
	r.noteFallbackWorktreeTitle(fbRepoPath, fbTitle)

	typedSegment := sessiongit.DerivedWorktreePathTitleSegment(typedRepo, typedTitle)
	fbSegment := sessiongit.DerivedWorktreePathTitleSegment(fbRepoPath, fbTitle)
	if typedSegment == "" || fbSegment == "" {
		t.Fatalf("DerivedWorktreePathTitleSegment empty: typed=%q fb=%q", typedSegment, fbSegment)
	}
	typedNeedle := typedRepo + "-" + typedSegment
	fbNeedle := fbRepoPath + "-" + fbSegment

	for _, needle := range []string{typedNeedle, fbNeedle} {
		if _, ok := r.worktreeTitleSiblingNeedles[needle]; !ok {
			t.Errorf("needle %q missing from worktreeTitleSiblingNeedles (set %v)",
				needle, r.worktreeTitleSiblingNeedles)
		}
		if !r.isWorktreeTitleSiblingNeedle(needle) {
			t.Errorf("isWorktreeTitleSiblingNeedle(%q) = false, want true", needle)
		}
	}
	// A non-needle (the repo_path alone, or a wrong segment) is not a sibling
	// needle, so it falls through to the bare blank in the matcher.
	for _, non := range []string{typedRepo, fbRepoPath, typedRepo + "-no-such-segment", fbRepoPath + "-no-such-segment"} {
		if r.isWorktreeTitleSiblingNeedle(non) {
			t.Errorf("isWorktreeTitleSiblingNeedle(%q) = true, want false", non)
		}
	}
	// The set mirrors worktreePathTitles: one needle per distinct pair.
	if len(r.worktreeTitleSiblingNeedles) != len(r.worktreePathTitles) {
		t.Fatalf("worktreeTitleSiblingNeedles size %d != worktreePathTitles size %d (the index must mirror the map)",
			len(r.worktreeTitleSiblingNeedles), len(r.worktreePathTitles))
	}
}
