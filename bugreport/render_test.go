package bugreport

import (
	"encoding/json"
	"testing"
)

func TestIndentRawPreservesMemberOrder(t *testing.T) {
	raw := json.RawMessage(`{"title":"lane","path":"/repo","status":2}`)
	want := "{\n  \"title\": \"lane\",\n  \"path\": \"/repo\",\n  \"status\": 2\n}"
	if got := string(indentRaw(raw)); got != want {
		t.Fatalf("indentRaw reordered a session payload:\n got: %s\nwant: %s", got, want)
	}
}
