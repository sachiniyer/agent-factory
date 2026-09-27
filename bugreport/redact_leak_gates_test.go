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
