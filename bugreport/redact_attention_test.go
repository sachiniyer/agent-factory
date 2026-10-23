package bugreport

import (
	"encoding/json"
	"strings"
	"testing"
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
