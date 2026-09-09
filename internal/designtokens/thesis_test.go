package designtokens

import (
	"encoding/json"
	"os"
	"testing"
)

// The caption is the smallest chrome text. The former 12px caption made branch
// and ownership metadata harder to read than the action it explains (#4065).
func TestThesisReadableCaption(t *testing.T) {
	raw, err := os.ReadFile("../../design/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	var tokens Tokens
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatal(err)
	}
	if got := tokens.Values["type-caption"].CSS; got != "0.8125rem" {
		t.Fatalf("smallest chrome text = %s; want 0.8125rem (13px)", got)
	}
}
