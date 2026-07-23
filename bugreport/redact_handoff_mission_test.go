package bugreport

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// TestRedactInstanceDataRedactsPendingHandoffMission is the #2419 regression
// guard for the typed path. PendingHandoffMission holds a rendered takeover
// brief that embeds the user's free-text prompt/goal verbatim — the same
// sensitivity class as Prompt, which redactInstanceData already blanks. The
// field was added with transactional handoff (#2286) after this redaction
// policy was written, so it passed through unredacted into publicly shared bug
// bundles.
func TestRedactInstanceDataRedactsPendingHandoffMission(t *testing.T) {
	d := session.InstanceData{
		ID:                    "abc123",
		Program:               "claude",
		Status:                session.Status(1),
		PendingHandoffMission: "continue the internal codename Kingfisher migration; secret runbook step 3",
	}

	redactInstanceData(&d)

	if d.PendingHandoffMission != redactedMarker {
		t.Errorf("pending handoff mission not redacted: %q", d.PendingHandoffMission)
	}
	// Structural fields still survive.
	if d.ID != "abc123" || d.Program != "claude" || d.Status != session.Status(1) {
		t.Errorf("structural fields mutated: %+v", d)
	}
}

// TestRedactInstancesFallbackRedactsPendingHandoffMission is the #2419 fallback
// guard. A legacy/corrupt record that fails the typed decode (here `status` is a
// string) must still have pending_handoff_mission redacted on the generic path.
// Before the fix the key was absent from sensitiveJSONKeys, so the handoff brief
// — which is not a secret pattern, path, or known title the text scrub would
// catch — leaked verbatim into the shared bundle.
func TestRedactInstancesFallbackRedactsPendingHandoffMission(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{"id":"leg-1","status":"legacy-string-status","program":"claude","pending_handoff_mission":"internal codename Kingfisher handoff brief"}]`)
	out := string(r.redactInstancesJSON(raw))
	if strings.Contains(out, "Kingfisher") {
		t.Errorf("fallback path leaked pending handoff mission:\n%s", out)
	}
	if !strings.Contains(out, redactedMarker) {
		t.Errorf("expected redaction marker on the fallback path:\n%s", out)
	}
	// Safe structural fields still survive.
	if !strings.Contains(out, "leg-1") || !strings.Contains(out, "legacy-string-status") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
}
