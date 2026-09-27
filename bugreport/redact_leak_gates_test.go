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
// blank must defer to the registered root (pathUnderRegisteredRoot skips it),
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
