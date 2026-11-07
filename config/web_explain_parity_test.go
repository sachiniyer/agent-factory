package config

// The web half of the comma-list contract (#6950): web/src/config.ts renders
// the explain trace through a key-aware formatter whose COMMA_LIST_KEYS set
// must equal isCommaListKey's opt-in set exactly. A comma-list key added on
// this side without updating the TS literal would silently keep its JSON
// spelling in the browser while the CLI and TUI comma-join it — the cross-
// surface divergence the issue exists to remove. The web bundle cannot import
// Go, so the drift check lives here: parse the literal out of the TS source
// and compare it against the real derivation over SettableKeys().

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

var (
	webCommaListLiteralRe = regexp.MustCompile(`(?s)COMMA_LIST_KEYS[^=]*=\s*new Set\(\[(.*?)\]\)`)
	webQuotedKeyRe        = regexp.MustCompile(`"([^"]+)"`)
)

func TestWebCommaListKeysMatchGoOptIn(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "web", "src", "config.ts"))
	if err != nil {
		t.Fatal(err)
	}
	m := webCommaListLiteralRe.FindSubmatch(src)
	if m == nil {
		t.Fatal("web/src/config.ts has no `COMMA_LIST_KEYS = new Set([...])` literal — the web explain formatter lost its key set, or renamed it without updating this test")
	}
	var web []string
	for _, q := range webQuotedKeyRe.FindAllSubmatch(m[1], -1) {
		web = append(web, string(q[1]))
	}
	var goSet []string
	for _, k := range SettableKeys() {
		if isCommaListKey(k) {
			goSet = append(goSet, k)
		}
	}
	sort.Strings(web)
	sort.Strings(goSet)
	if !reflect.DeepEqual(web, goSet) {
		t.Fatalf("web COMMA_LIST_KEYS drifted from isCommaListKey's opt-in set:\n  web: %v\n  go:  %v\nUpdate the literal in web/src/config.ts so the browser trace comma-joins the same keys the CLI and TUI do.", web, goSet)
	}
}
