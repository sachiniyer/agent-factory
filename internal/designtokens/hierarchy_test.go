package designtokens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hierarchyFixture(t *testing.T) Tokens {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "design", "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tokens Tokens
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatal(err)
	}
	return tokens
}

func setColorMode(tokens *Tokens, role, mode, value string) {
	c := tokens.Colors[role]
	if mode == "light" {
		c.Light = value
	} else {
		c.Dark = value
	}
	tokens.Colors[role] = c
}

func TestInkHierarchyBothThemes(t *testing.T) {
	for _, mode := range []string{"light", "dark"} {
		t.Run(mode+" insufficient separation", func(t *testing.T) {
			tokens := hierarchyFixture(t)
			// These both remain readable on the background but fail the ink hierarchy.
			value := "#3b4252"
			if mode == "dark" {
				value = "#d8dee9"
			}
			setColorMode(&tokens, "ink-muted", mode, value)
			err := validate(tokens)
			if err == nil || !strings.Contains(err.Error(), "hierarchy") {
				t.Fatalf("expected hierarchy rejection, got %v", err)
			}
		})
		t.Run(mode+" reversed emphasis", func(t *testing.T) {
			tokens := hierarchyFixture(t)
			ink, muted := tokens.Colors["ink"].value(mode), tokens.Colors["ink-muted"].value(mode)
			setColorMode(&tokens, "ink", mode, muted)
			setColorMode(&tokens, "ink-muted", mode, ink)
			err := validate(tokens)
			if err == nil || !strings.Contains(err.Error(), "secondary") {
				t.Fatalf("expected reversed hierarchy rejection, got %v", err)
			}
		})
	}
}

func TestMutedSharingRequiresPerThemeIntent(t *testing.T) {
	for _, mode := range []string{"light", "dark"} {
		t.Run(mode, func(t *testing.T) {
			tokens := hierarchyFixture(t)
			setColorMode(&tokens, "ready", mode, strings.ToUpper(tokens.Colors["ink-muted"].value(mode)))
			err := validate(tokens)
			if err == nil || !strings.Contains(err.Error(), "record an intentional sharesMuted") {
				t.Fatalf("accepted accidental sharing: %v", err)
			}
			c := tokens.Colors["ready"]
			c.SharesMuted = map[string]string{mode: "Explicit test-only sharing decision"}
			tokens.Colors["ready"] = c
			if err := validate(tokens); err != nil {
				t.Fatalf("rejected explicit sharing: %v", err)
			}
			c.SharesMuted[mode] = " "
			if err := validate(tokens); err == nil {
				t.Fatal("accepted an empty rationale")
			}
		})
	}
}

func TestMutedSharingRejectsStaleOrInvalidMarks(t *testing.T) {
	for name, mutate := range map[string]func(*Tokens){
		"removed intent": func(v *Tokens) { c := v.Colors["running"]; c.SharesMuted = nil; v.Colors["running"] = c },
		"changed value":  func(v *Tokens) { setColorMode(v, "archived", "light", v.Colors["ink"].Light) },
		"wrong theme":    func(v *Tokens) { c := v.Colors["running"]; c.SharesMuted["system"] = "System is not a palette" },
		"non-state role": func(v *Tokens) {
			c := v.Colors["border"]
			c.SharesMuted = map[string]string{"light": "Not liveness"}
			v.Colors["border"] = c
		},
	} {
		t.Run(name, func(t *testing.T) {
			tokens := hierarchyFixture(t)
			mutate(&tokens)
			if err := validate(tokens); err == nil {
				t.Fatal("accepted invalid sharing metadata")
			}
		})
	}
}
