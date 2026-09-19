package redactx

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/redactspan"
)

// TestURIMalformedQueryFragmentAdmitted pins the contract that a malformed
// percent escape inside a parser-proven URI query pair or fragment never
// causes the component's stable view to be dropped. percent.go documents
// that malformed bytes are ordinary data in the stable view; the URI
// transform must hand that view to the producer (or fail-close the range),
// never silently discard it. A discarded view is invisible to both the
// producer and the engine's fail-closed path, so a credential that sits
// under one valid escape (defeating the outer shape regex) plus one
// malformed escape rides through to the output untouched.
func TestURIMalformedQueryFragmentAdmitted(t *testing.T) {
	const sentinel = "ZZSECRETZZ"
	const marker = "[s]"
	// rawSpelling is the recoverable percent-encoded surface of the secret:
	// one byte ('S') is escaped (%53) so the plaintext sentinel does not
	// appear in the raw text. A reader URL-decoding the output would still
	// recover the sentinel from this surface.
	const rawSpelling = "ZZ%53ECRETZZ"

	e := &Engine{
		Produce: func(text string, _ Provenance) []redactspan.Span {
			// The producer runs on every admitted view. It sees the sentinel
			// only once the URI component has been decoded; the raw,
			// percent-encoded surface carries no plaintext sentinel.
			if i := strings.Index(text, sentinel); i >= 0 {
				return []redactspan.Span{{Start: i, End: i + len(sentinel), Replacement: marker, Priority: 1}}
			}
			return nil
		},
		FailClosed: redactspan.Span{Replacement: "[f]", Priority: 2},
		Fallback:   "[f]",
	}

	t.Run("query-malformed", func(t *testing.T) {
		got := e.Scrub("http://h/?k="+rawSpelling+"%2g", ProvLogRecord)
		want := "http://h/?k=" + marker + "%2g"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("fragment-malformed", func(t *testing.T) {
		got := e.Scrub("http://h/p#"+rawSpelling+"%2g", ProvLogRecord)
		want := "http://h/p#" + marker + "%2g"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	// Well-formed controls: the view path already worked before the fix;
	// these prove the fix did not regress it.
	t.Run("query-wellformed", func(t *testing.T) {
		got := e.Scrub("http://h/?k="+rawSpelling+" ok", ProvLogRecord)
		want := "http://h/?k=" + marker + " ok"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("fragment-wellformed", func(t *testing.T) {
		got := e.Scrub("http://h/p#"+rawSpelling, ProvLogRecord)
		want := "http://h/p#" + marker
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	// A different malformed spelling (%ZZ: 'Z' is not a hex digit) confirms
	// the fix is not specific to %2g.
	t.Run("query-malformed-percent-ZZ", func(t *testing.T) {
		got := e.Scrub("http://h/?k="+rawSpelling+"%ZZ", ProvLogRecord)
		if strings.Contains(got, sentinel) || strings.Contains(got, rawSpelling) {
			t.Fatalf("sentinel survived: %q", got)
		}
		if !strings.Contains(got, marker) || !strings.Contains(got, "%ZZ") {
			t.Fatalf("want %q and %q in output, got %q", marker, "%ZZ", got)
		}
	})
	// A secret in the SECOND query pair, where the second pair is the
	// malformed one: splitQueryPairs must decode and admit each pair on
	// its own.
	t.Run("query-malformed-second-pair", func(t *testing.T) {
		got := e.Scrub("http://h/?a=1&k="+rawSpelling+"%2g", ProvLogRecord)
		want := "http://h/?a=1&k=" + marker + "%2g"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	// A malformed fragment with NO secret underneath must remain
	// byte-for-byte: admitting the view must not invent a redaction or
	// fail-close over inert bytes. This is the transform-layer mirror of
	// bugreport's malformed_fragment_cannot_revoke_path_evidence policy.
	t.Run("inert-malformed-fragment-unchanged", func(t *testing.T) {
		in := "http://h/p#frag%ZZ"
		got := e.Scrub(in, ProvLogRecord)
		if got != in {
			t.Fatalf("inert malformed fragment rewritten: got %q, want %q", got, in)
		}
	})

	// Direct contract assertion: sourceMappedURIComponents must return a
	// view for the malformed query and fragment — the exact thing the bug
	// dropped silently.
	t.Run("transform-admits-malformed-query-view", func(t *testing.T) {
		views, _ := sourceMappedURIComponents(Identity("http://h/?k="+rawSpelling+"%2g"), ProvLogRecord)
		if !viewHas(views, ProvURIQueryPair, "k=ZZSECRETZZ%2g") {
			t.Fatalf("malformed query view not admitted: %+v", views)
		}
	})
	t.Run("transform-admits-malformed-fragment-view", func(t *testing.T) {
		views, _ := sourceMappedURIComponents(Identity("http://h/p#"+rawSpelling+"%2g"), ProvLogRecord)
		if !viewHas(views, ProvURIComponent, "ZZSECRETZZ%2g") {
			t.Fatalf("malformed fragment view not admitted: %+v", views)
		}
	})
}

// viewHas reports whether views contains an entry with the given provenance
// and decoded text.
func viewHas(views []viewOut, prov Provenance, want string) bool {
	for _, w := range views {
		if w.prov == prov && w.view.Text == want {
			return true
		}
	}
	return false
}
