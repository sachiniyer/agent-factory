package bugreport

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnknownJSONRedactsProposalSlug(t *testing.T) {
	input := map[string]any{"tabs": []any{map[string]any{"kind": 5, "proposal": "secret-launch-plan"}}}
	out, err := json.Marshal(redactUnknownJSON(input))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "secret-launch-plan") {
		t.Fatalf("the fallback path kept the proposal slug: %s", out)
	}
}
