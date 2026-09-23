package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The shared explain seam (#4803): one resolution every surface renders — the
// CLI's --explain, the daemon's ExplainConfig route, and through it the TUI and
// web. These tests pin the SEAM, not the resolver (provenance.go's own tests
// cover the trace semantics); a seam that drifted would let the surfaces
// disagree about which layer supplied a value.

func TestExplainGlobalValueResolvesTheSameTraceTheCLIPrints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	v, err := ExplainGlobalValue("default_program")
	if err != nil {
		t.Fatalf("ExplainGlobalValue: %v", err)
	}
	if v.Key != "default_program" {
		t.Fatalf("explained the wrong key: %q", v.Key)
	}
	if v.Value != "claude" {
		t.Fatalf("an untouched key must resolve to its default, got %v", v.Value)
	}
	if len(v.Candidates) == 0 {
		t.Fatal("no candidates — the trace is the capability, not the value")
	}
	// Exactly one candidate wins. Which layer that is on a fresh home is the
	// loader's business — it may seed a default config.toml, making the global
	// layer rather than the built-in the winner — but every surface sees the
	// SAME answer through this one call.
	winners := 0
	for _, cand := range v.Candidates {
		if cand.Result == "winner" {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("a replace-merged key must name exactly one winner, got %+v", v.Candidates)
	}
}

func TestExplainGlobalValueNamesTheLayerThatWon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("default_program = \"codex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	v, err := ExplainGlobalValue("default_program")
	if err != nil {
		t.Fatalf("ExplainGlobalValue: %v", err)
	}
	if v.Value != "codex" {
		t.Fatalf("the file's value must win, got %v", v.Value)
	}
	if v.Winner == nil || v.Winner.Layer != "global" || v.Winner.Path == "" {
		t.Fatalf("the global layer must be named the winner with its path, got %+v", v.Winner)
	}
	// The built-in is still IN the trace — shadowed, not dropped. An explain
	// that hid the losing candidates would answer "what won" while hiding
	// "what it beat", which is the question the surface exists for.
	var builtIn *CandidateTrace
	for i := range v.Candidates {
		if v.Candidates[i].Layer == "built-in" {
			builtIn = &v.Candidates[i]
		}
	}
	if builtIn == nil || builtIn.Result != "shadowed" {
		t.Fatalf("the built-in candidate must be shadowed, not absent from the trace: %+v", v.Candidates)
	}
}

func TestExplainGlobalValueRejectsUnknownKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	if _, err := ExplainGlobalValue("not.a.key"); err == nil {
		t.Fatal("an unknown key must be refused, not explained")
	} else if got := err.Error(); got == "" || got == "not.a.key" {
		t.Fatalf("the refusal must say WHAT was unknown, got %q", got)
	}
}

func TestFormatExplainValueSpellsValuesLikeTheExplanation(t *testing.T) {
	// An explicitly configured empty string stays visible — a bare blank would
	// read as absent, the exact confusion --explain exists to clear up.
	if got := FormatExplainValue(""); got != `""` {
		t.Fatalf("empty string must render %q, got %q", `""`, got)
	}
	if got := FormatExplainValue("codex"); got != "codex" {
		t.Fatalf("scalars print bare, got %q", got)
	}
	if got := FormatExplainValue(true); got != "true" {
		t.Fatalf("bool prints bare, got %q", got)
	}
	if got := FormatExplainValue(map[string]string{"claude": "/usr/bin/claude"}); got != `{"claude":"/usr/bin/claude"}` {
		t.Fatalf("composites print compact JSON, got %q", got)
	}
}

func TestFormatValueIsTheBareScalarRenderer(t *testing.T) {
	// `af config get` prints a bare value for scripting — this is the one
	// renderer behind it, shared with the explain surface so no surface spells
	// the same value differently.
	if got := FormatValue("claude"); got != "claude" {
		t.Fatalf("got %q", got)
	}
	if got := FormatValue(4); got != "4" {
		t.Fatalf("got %q", got)
	}
}
