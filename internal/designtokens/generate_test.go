package designtokens

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCommittedArtifacts(t *testing.T) {
	root := filepath.Join("..", "..")
	out, err := Generate(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(root, out); err != nil {
		t.Fatal(err)
	}
	again, err := Generate(root)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range out {
		if !bytes.Equal(want, again[p]) {
			t.Fatalf("nondeterministic %s", p)
		}
	}
	if !bytes.Equal(out["web/src/tokens.css"], out["docs/stylesheets/tokens.css"]) {
		t.Fatal("docs CSS differs")
	}
}

func TestDriftRejectsEveryMissingOrStaleOutput(t *testing.T) {
	out, err := Generate(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for p := range out {
		t.Run(p, func(t *testing.T) {
			root := t.TempDir()
			if err := Write(root, out); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, p)
			if err := os.WriteFile(target, []byte("stale"), 0600); err != nil {
				t.Fatal(err)
			}
			if Check(root, out) == nil {
				t.Fatal("accepted stale output")
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if Check(root, out) == nil {
				t.Fatal("accepted missing output")
			}
			if err := Write(root, out); err != nil {
				t.Fatal(err)
			}
			if err := Check(root, out); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRejectInvalidContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "design", "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Tokens){
		"extra colour":                      func(v *Tokens) { v.Colors["selection"] = v.Colors["surface"] },
		"extra metric":                      func(v *Tokens) { v.Values["space-5"] = v.Values["space-4"] },
		"missing metric":                    func(v *Tokens) { delete(v.Values, "space-4") },
		"running indicator":                 func(v *Tokens) { v.States[0].Glyph = "●" },
		"duplicate state":                   func(v *Tokens) { v.States[1] = v.States[0] },
		"wrong state color":                 func(v *Tokens) { v.States[1].Color = "lost" },
		"missing state color":               func(v *Tokens) { v.States[0].Color = "absent" },
		"dark border below unrounded floor": func(v *Tokens) { c := v.Colors["border"]; c.Dark = "#3c3c3c"; v.Colors["border"] = c },
		"body ink below seven":              func(v *Tokens) { c := v.Colors["ink"]; c.Dark = "#999999"; v.Colors["ink"] = c },
		"unreadable ink":                    func(v *Tokens) { v.Colors["ink"] = v.Colors["surface"] },
		"unsafe CSS":                        func(v *Tokens) { v.Values["space-1"] = Value{CSS: "0; } body { color:red", Role: "bad"} },
	} {
		t.Run(name, func(t *testing.T) {
			var v Tokens
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			mutate(&v)
			if validate(v) == nil {
				t.Fatal("accepted invalid contract")
			}
		})
	}
}
