package namegen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// wordRe pins every wordlist entry to a single lowercase [a-z] word. This is the
// property that lets a joined "adjective-noun" be used verbatim as a session
// title: it sanitizes to itself as a git branch and a tmux name, and can never
// equal the reserved "root" title or need trimming.
var wordRe = regexp.MustCompile(`^[a-z]+$`)

func TestWordlistsAreCleanSlugs(t *testing.T) {
	for _, group := range []struct {
		name  string
		words []string
	}{
		{"adjective", adjectives},
		{"noun", nouns},
	} {
		seen := map[string]bool{}
		for _, w := range group.words {
			if !wordRe.MatchString(w) {
				t.Errorf("%s %q is not a single lowercase [a-z] word", group.name, w)
			}
			if session.IsReservedTitle(w) {
				t.Errorf("%s %q collides with the reserved root title", group.name, w)
			}
			if seen[w] {
				t.Errorf("%s %q is duplicated in its list", group.name, w)
			}
			seen[w] = true
		}
		if len(group.words) < 2 {
			t.Errorf("%s list is too small (%d) to vary names", group.name, len(group.words))
		}
	}
}

// TestGeneratedNameIsNeverReserved is the belt-and-suspenders check that no
// adjective-noun pair (not just the individual words) reads as the root title.
func TestGeneratedNameIsNeverReserved(t *testing.T) {
	for _, a := range adjectives {
		for _, n := range nouns {
			name := a + "-" + n
			if session.IsReservedTitle(name) {
				t.Fatalf("generated name %q is reserved", name)
			}
			if !wordRe.MatchString(strings.ReplaceAll(name, "-", "")) {
				t.Fatalf("generated name %q is not a clean slug", name)
			}
		}
	}
}

// seqGen returns a Generator whose index picks walk a fixed sequence, so the
// exact names it produces are deterministic.
func seqGen(seq ...int) *Generator {
	i := 0
	return &Generator{intn: func(n int) int {
		v := seq[i%len(seq)] % n
		i++
		return v
	}}
}

func TestRandomFormat(t *testing.T) {
	// First pick indexes adjectives[0], second indexes nouns[0].
	got := seqGen(0, 0).Random()
	want := adjectives[0] + "-" + nouns[0]
	if got != want {
		t.Fatalf("Random() = %q, want %q", got, want)
	}
	if len(strings.Split(got, "-")) != 2 {
		t.Fatalf("Random() = %q, want exactly one hyphen", got)
	}
}

func TestSuggestSkipsTakenNames(t *testing.T) {
	// Sequence yields adjectives[0]-nouns[0], then adjectives[1]-nouns[1], …
	g := seqGen(0, 0, 1, 1, 2, 2)
	first := adjectives[0] + "-" + nouns[0]
	second := adjectives[1] + "-" + nouns[1]
	taken := map[string]bool{first: true}
	got := g.Suggest(func(name string) bool { return taken[name] })
	if got != second {
		t.Fatalf("Suggest skipped to %q, want %q (the first free name)", got, second)
	}
}

func TestSuggestNilTakenReturnsFirst(t *testing.T) {
	got := seqGen(3, 4).Suggest(nil)
	want := adjectives[3] + "-" + nouns[4]
	if got != want {
		t.Fatalf("Suggest(nil) = %q, want %q", got, want)
	}
}

func TestSuggestGivesUpButStaysValid(t *testing.T) {
	// taken is always true: Suggest exhausts maxAttempts and returns its last
	// candidate anyway — still a well-formed name (the daemon's create-time
	// auto-suffix, not this loop, guarantees final uniqueness).
	got := seqGen(0, 0).Suggest(func(string) bool { return true })
	if !wordRe.MatchString(strings.ReplaceAll(got, "-", "")) || len(strings.Split(got, "-")) != 2 {
		t.Fatalf("Suggest gave up with a malformed name %q", got)
	}
}

func TestPackageSuggestUsesSharedGenerator(t *testing.T) {
	// The exported helpers must actually draw from the wordlists.
	name := Suggest(nil)
	parts := strings.Split(name, "-")
	if len(parts) != 2 {
		t.Fatalf("Suggest(nil) = %q, want adjective-noun", name)
	}
	adj := map[string]bool{}
	for _, a := range adjectives {
		adj[a] = true
	}
	noun := map[string]bool{}
	for _, n := range nouns {
		noun[n] = true
	}
	if !adj[parts[0]] || !noun[parts[1]] {
		t.Fatalf("Suggest(nil) = %q, not drawn from the wordlists", name)
	}
}
