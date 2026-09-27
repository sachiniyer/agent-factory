package bugreport

import (
	"encoding/json"
	"fmt"
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
