package designtokens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// Read the actual embedded artifacts, without Generate, a browser, or theme.ts.
// This runs in the existing Go suite that gates Build, and explicitly in Lint.
func TestDesignWebChromeServedBytes(t *testing.T) {
	read := func(path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var tokens Tokens
	if err := json.Unmarshal(read("design/tokens.json"), &tokens); err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(strings.NewReader(string(read("web/dist/index.html"))))
	if err != nil {
		t.Fatal(err)
	}
	metas := map[string][]string{}
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "meta" {
			attrs := map[string]string{}
			for _, attr := range n.Attr {
				attrs[attr.Key] = attr.Val
			}
			if attrs["name"] == "theme-color" {
				metas[attrs["media"]] = append(metas[attrs["media"]], attrs["content"])
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(doc)
	if len(metas) != 2 {
		t.Errorf("index.html: want exactly two theme-color schemes, got %v", metas)
	}
	for _, mode := range []string{"light", "dark"} {
		got := metas["(prefers-color-scheme: "+mode+")"]
		want := tokens.Colors["surface"].value(mode)
		if len(got) != 1 || !strings.EqualFold(got[0], want) {
			t.Errorf("index.html %s theme-color = %v, want %s", mode, got, want)
		}
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(read("web/dist/manifest.webmanifest"), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"theme_color", "background_color"} {
		var got string
		if err := json.Unmarshal(manifest[key], &got); err != nil {
			t.Fatalf("manifest %s: %v", key, err)
		}
		if want := tokens.Colors["surface"].Light; !strings.EqualFold(got, want) {
			t.Errorf("manifest %s = %s, want light surface %s", key, got, want)
		}
	}
}

func TestDesignWebStampRejectsMissingOrDuplicateSlots(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "design/tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tokens Tokens
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"web/src/index.html", "web/src/manifest.webmanifest"} {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, path))
			if err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string][]byte{"missing": []byte(""), "duplicate": append(append([]byte{}, raw...), raw...)} {
				t.Run(name, func(t *testing.T) {
					dir := t.TempDir()
					if err := Write(dir, map[string][]byte{path: content}); err != nil {
						t.Fatal(err)
					}
					generate := webShell
					if strings.HasSuffix(path, ".webmanifest") {
						generate = webManifest
					}
					if _, err := generate(dir, tokens); err == nil {
						t.Fatal("accepted malformed color slots")
					}
				})
			}
		})
	}
}
