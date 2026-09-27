package bugreport

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestLeakRepro reproduces the daemon-log-tail path leak from noteUnknownJSONRecord.
// On the fallback path the gathered repo_path is never registered as a root and
// never redacted for the log scrub, so the verbatim path (and the private directory
// leaf) survives scrubLog.
func TestLeakRepro(t *testing.T) {
	r := &redactor{}
	r.noteAFHome(siblingLeakAFHome)
	raw := json.RawMessage(fmt.Sprintf(`[{
		"status": "done",
		"title": %q,
		"worktree": {"repo_path": %q, "worktree_path": %q,
			"relocation_recovery": {"alternate_path": %q}}}]`,
		siblingLeakTitle, siblingLeakRepo, siblingLeakArchive, siblingLeakAlternate))
	r.redactInstancesJSON(raw)
	got := r.scrubLog(siblingRecoveryLogLine())
	t.Logf("scrubbed:\n%s", got)
	for _, secret := range []string{siblingLeakRepo, "ConfidentialClient"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q", secret)
		}
	}
}
