package bugreport

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

func TestRedactInstancesFallbackRedactsAttention(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{"id":"leg-1","status":"legacy-string-status","attention":{"reason":"approve the Kingfisher merger","set_at":"2026-10-22T00:00:00Z"}}]`)
	out := string(r.redactInstancesJSON(raw))
	if strings.Contains(out, "Kingfisher") {
		t.Errorf("fallback path leaked the attention reason:\n%s", out)
	}
	if !strings.Contains(out, "leg-1") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
}

// #7635: the two fields a daemon-raised held-dialog notice persists. Origin
// is published only when it is a value af itself writes; anything else found
// in the field on disk is marked. The flagged pane's key is a host-authored
// digest of pane content and is always marked.
func TestRedactInstanceDataBoundsTheHeldDialogMarks(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	row := func(origin string) session.InstanceData {
		return session.InstanceData{
			ID:                "inst-1",
			Attention:         &session.Attention{Reason: "approve the Kingfisher merger", Origin: origin},
			DialogHoldFlagged: key,
		}
	}

	redactInstanceData := func(d session.InstanceData) session.InstanceData {
		(&redactor{}).redactInstanceData(&d)
		return d
	}

	out := redactInstanceData(row(session.AttentionOriginDialogHold))
	if out.Attention == nil || out.Attention.Origin != session.AttentionOriginDialogHold {
		t.Errorf("af's own origin enum was not kept: %+v", out.Attention)
	}
	if out.Attention.Reason != redactedMarker {
		t.Errorf("the reason was not redacted: %q", out.Attention.Reason)
	}
	if out.DialogHoldFlagged != redactedMarker {
		t.Errorf("the flagged pane's key reached the report: %q", out.DialogHoldFlagged)
	}

	out = redactInstanceData(row("Kingfisher"))
	if out.Attention.Origin != redactedMarker {
		t.Errorf("an origin af does not write reached the report: %q", out.Attention.Origin)
	}

	// The agent's own request has no origin, and none is invented for it.
	out = redactInstanceData(row(""))
	if out.Attention.Origin != "" {
		t.Errorf("an empty origin was changed: %q", out.Attention.Origin)
	}

	// A row with neither mark is left without them.
	if out = redactInstanceData(session.InstanceData{ID: "inst-2"}); out.Attention != nil || out.DialogHoldFlagged != "" {
		t.Errorf("marks were invented on a row that had none: %+v, %q", out.Attention, out.DialogHoldFlagged)
	}
}

// The fallback path, for a row the typed decode rejects, is no less private.
func TestRedactInstancesFallbackRedactsTheHeldDialogMarks(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{"id":"leg-1","status":"legacy-string-status","dialog_hold_flagged":"0123456789abcdefKingfisher","attention":{"reason":"x","origin":"Kingfisher","set_at":"2026-10-22T00:00:00Z"}}]`)
	out := string(r.redactInstancesJSON(raw))
	if strings.Contains(out, "Kingfisher") || strings.Contains(out, "0123456789abcdef") {
		t.Errorf("fallback path leaked a held-dialog mark:\n%s", out)
	}
	if !strings.Contains(out, "leg-1") || !strings.Contains(out, "dialog_hold_flagged") {
		t.Errorf("fallback dropped safe structure:\n%s", out)
	}
}
