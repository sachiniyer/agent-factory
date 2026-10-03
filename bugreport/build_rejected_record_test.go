package bugreport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildEndToEndRejectedRecordLogLeak reproduces the noteUnknownJSONRecord
// daemon-log-tail leak through the PRODUCTION Build path (real newRedactor), not
// a stripped &redactor{}.
//
// Scenario:
//   - repo A's instances.json is a structurally valid v1 array but one record
//     mistypes a field ("status":"done" where session.Status is an int), so the
//     typed []InstanceData decode rejects the record and noteUnknownJSONRecord
//     runs. #4115 deliberately registers no root for the untyped repo_path.
//   - a task exists for a DIFFERENT repo B, so gate 2 (no cross-section root
//     registration of A's repo_path) stays open: no task ProjectPath resolves to
//     /srv/ConfidentialClient/repo.
//   - the daemon log tail carries A's repo_path verbatim.
//
// Asserts the verbatim path and the private leaf survive in none of res.Text,
// res.JSON, or res.Body. This is #3588's "hold across every section at once"
// parity contract on the fallback path; the typed path upholds it via
// noteRepoRoot, and the log-scope blank now upholds it on the fallback without
// registering the untyped value as a root (#4115).
func TestBuildEndToEndRejectedRecordLogLeak(t *testing.T) {
	home := t.TempDir()
	afHome := filepath.Join(home, ".agent-factory")
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	if err := os.MkdirAll(afHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// Repo A: a rejected-but-valid instances.json. "status":"done" is a string
	// where session.Status is an int, so []InstanceData decode fails and the
	// generic fallback runs. The record's repo_path is deliberately outside $HOME
	// so the home->~ collapse and the username sweep cannot touch it; only the
	// fallback's log-scope blank can close the leak.
	instDirA := filepath.Join(afHome, "instances", "repoA")
	if err := os.MkdirAll(instDirA, 0o755); err != nil {
		t.Fatal(err)
	}
	rejected := `[{ "id": "inst-a1", "title": "secret-proj", "status": "done",
		"worktree": { "repo_path": "/srv/ConfidentialClient/repo",
			"worktree_path": "/srv/ConfidentialClient/repo-wt" } }]`
	writeFile(t, filepath.Join(instDirA, "instances.json"), rejected)

	// A task bound to a DIFFERENT repo B so gate 2 stays open for A: no task
	// ProjectPath resolves to /srv/ConfidentialClient/repo, so redactTasks never
	// registers it as a root.
	tasks := `[{"id": "tB", "name": "other", "prompt": "x", "cron_expr": "0 9 * * *",
		"target_session": "other-target", "enabled": true,
		"project_path": "` + home + `/other-repo", "program": "claude"}]`
	writeFile(t, filepath.Join(afHome, "tasks.json"), tasks)

	// Daemon log tail carrying A's repo_path verbatim (the WORKTREE_MISSING_DETECTED
	// shape, one of several emitters that interpolate a raw repo_path via %q).
	logLine := `2026-09-27 WORKTREE_MISSING_DETECTED classification="missing" title="secret-proj" instance_id="inst-a1" repo_id="repoA" repo_path="/srv/ConfidentialClient/repo" worktree_path="/srv/ConfidentialClient/repo-wt" recover_error="either /srv/ConfidentialClient/repo-wt or /srv/ConfidentialClient/repo-secret-proj (identity unresolved)"` + "\n"
	writeFile(t, filepath.Join(afHome, "agent-factory.log"), logLine)

	res, err := Build(Inputs{
		AFVersion:    "9.9.9",
		GeneratedAt:  "2026-09-27 00:00:00 +0000",
		DaemonStatus: map[string]any{"running": false},
		DaemonHuman:  "daemon: not running\n",
		BundlePath:   filepath.Join(home, "af-bug-report.txt"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	leaked := []string{"/srv/ConfidentialClient/repo", "ConfidentialClient"}
	for _, secret := range leaked {
		if strings.Contains(res.Text, secret) {
			t.Errorf("[text] leaked %q through production Build (newRedactor):\n%s", secret, res.Text)
		}
		if strings.Contains(string(res.JSON), secret) {
			t.Errorf("[json] leaked %q through production Build (newRedactor):\n%s", secret, res.JSON)
		}
		if strings.Contains(res.Body, secret) {
			t.Errorf("[body] leaked %q through production Build (newRedactor):\n%s", secret, res.Body)
		}
	}

	// Repo B's task registers its own project_path as [repo:1]; that is the
	// typed path's legitimate root and is expected. The #4115 guarantee — that
	// the FALLBACK does not number the rejected record's untyped repo_path — is
	// pinned in TestScrubLogRedactsSanitizedTitleFromRejectedRecord, which
	// runs with no task registered so [repo:1] can only come from the fallback.
	// Here, confirm the rejected record's repo_path blanked to the marker in
	// the log section (not collapsed to a numbered token that would stand in
	// for the untyped value).
	if !strings.Contains(res.Text, `repo_path="[redacted]"`) {
		t.Errorf("[text] rejected record repo_path was not blanked to [redacted] in the log section:\n%s", res.Text)
	}
}
