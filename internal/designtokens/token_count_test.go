package designtokens

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocsStateTheTokenTotal(t *testing.T) {
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	var tokens Tokens
	if err := json.Unmarshal([]byte(read("design/tokens.json")), &tokens); err != nil {
		t.Fatal(err)
	}
	total := len(tokens.Colors) + len(tokens.Values)
	for rel, want := range map[string]string{
		"design/style-guide.tmpl":         fmt.Sprintf("Exact internal values · %d tokens", total),
		"docs/design/interface-design.md": fmt.Sprintf("exactly %d custom properties per theme", total),
	} {
		if !strings.Contains(read(rel), want) {
			t.Errorf("%s does not say %q", rel, want)
		}
	}
}
