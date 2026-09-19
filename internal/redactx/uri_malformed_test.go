package redactx

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/redactspan"
)

// TestURIMalformedQueryFragmentFailClosed pins the fail-close contract for a
// malformed percent escape inside a parser-proven URI query pair or fragment:
// the component's source range routes into unknown (fail-closed by the engine)
// rather than being silently dropped, so a credential that sits under one
// valid escape (defeating the outer shape regex) plus one malformed escape
// cannot ride through to the output. This inverts the keep-view predecessor
// (uri_malformed_test.go on the superseded #4660): the adversarial inputs — a
// sentinel with one byte percent-escaped plus a trailing malformed escape,
// the %ZZ spelling, a second &-separated query pair, and an inert malformed
// fragment — are retained; the assertions flip from "the view is admitted and
// the malformed tail ships as ordinary data" to "no view is admitted and the
// whole component fails closed, tail included".
func TestURIMalformedQueryFragmentFailClosed(t *testing.T) {
	const sentinel = "ZZSECRETZZ"
	const marker = "[s]"
	// rawSpelling is the recoverable percent-encoded surface of the secret:
	// one byte ('S') is escaped (%53) so the plaintext sentinel does not
	// appear in the raw text. A reader URL-decoding the output would still
	// recover the sentinel from this surface unless the range fails closed.
	const rawSpelling = "ZZ%53ECRETZZ"

	e := &Engine{
		Produce: func(text string, _ Provenance) []redactspan.Span {
			// The producer runs only on an admitted view. Under fail-close a
			// malformed component produces no view, so the producer never
			// sees it and the engine redacts the whole source range.
			if i := strings.Index(text, sentinel); i >= 0 {
				return []redactspan.Span{{Start: i, End: i + len(sentinel), Replacement: marker, Priority: 1}}
			}
			return nil
		},
		FailClosed: redactspan.Span{Replacement: "[f]", Priority: 2},
		Fallback:   "[f]",
	}

	// Malformed query pair: the whole pair (secret and the trailing %2g)
	// fails closed. The sentinel, its percent-encoded surface, and the
	// malformed tail must all be absent; the fail-close marker must appear.
	t.Run("query-malformed", func(t *testing.T) {
		got := e.Scrub("http://h/?k="+rawSpelling+"%2g", ProvLogRecord)
		if strings.Contains(got, sentinel) || strings.Contains(got, rawSpelling) || strings.Contains(got, "%2g") {
			t.Fatalf("malformed query shipped recoverable bytes: %q", got)
		}
		if !strings.Contains(got, "[f]") {
			t.Fatalf("malformed query not fail-closed: %q", got)
		}
	})
	// Malformed fragment: same contract as the query pair.
	t.Run("fragment-malformed", func(t *testing.T) {
		got := e.Scrub("http://h/p#"+rawSpelling+"%2g", ProvLogRecord)
		if strings.Contains(got, sentinel) || strings.Contains(got, rawSpelling) || strings.Contains(got, "%2g") {
			t.Fatalf("malformed fragment shipped recoverable bytes: %q", got)
		}
		if !strings.Contains(got, "[f]") {
			t.Fatalf("malformed fragment not fail-closed: %q", got)
		}
	})
	// Well-formed controls: the view path already worked before the fix and
	// fail-close does not touch it, so the sentinel is redacted through the
	// producer and the surrounding bytes pass through unchanged.
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
	// the fail-close is not specific to %2g. Under the keep-view design the
	// %ZZ tail survived; under fail-close the whole pair is redacted.
	t.Run("query-malformed-percent-ZZ", func(t *testing.T) {
		got := e.Scrub("http://h/?k="+rawSpelling+"%ZZ", ProvLogRecord)
		if strings.Contains(got, sentinel) || strings.Contains(got, rawSpelling) || strings.Contains(got, "%ZZ") {
			t.Fatalf("malformed query (%%ZZ) shipped recoverable bytes: %q", got)
		}
		if !strings.Contains(got, "[f]") {
			t.Fatalf("malformed query (%%ZZ) not fail-closed: %q", got)
		}
	})
	// A secret in the SECOND query pair, where the second pair is the
	// malformed one: splitQueryPairs decodes each pair on its own, so the
	// well-formed first pair (a=1) survives and the malformed second pair
	// fails closed.
	t.Run("query-malformed-second-pair", func(t *testing.T) {
		got := e.Scrub("http://h/?a=1&k="+rawSpelling+"%2g", ProvLogRecord)
		if strings.Contains(got, sentinel) || strings.Contains(got, rawSpelling) || strings.Contains(got, "%2g") {
			t.Fatalf("second malformed pair shipped recoverable bytes: %q", got)
		}
		if !strings.Contains(got, "a=1") || !strings.Contains(got, "[f]") {
			t.Fatalf("well-formed first pair or fail-close marker missing: %q", got)
		}
	})

	// A malformed fragment with NO secret underneath must fail closed too.
	// The keep-view design left inert malformed bytes byte-for-byte; under
	// fail-close the engine redacts the undecodable fragment regardless of
	// whether it carries a secret.
	t.Run("inert-malformed-fragment-failclosed", func(t *testing.T) {
		in := "http://h/p#frag%ZZ"
		got := e.Scrub(in, ProvLogRecord)
		if strings.Contains(got, "frag%ZZ") {
			t.Fatalf("inert malformed fragment bytes survived: %q", got)
		}
		if !strings.Contains(got, "[f]") {
			t.Fatalf("inert malformed fragment not fail-closed: %q", got)
		}
	})

	// Direct contract assertion: sourceMappedURIComponents must NOT admit a
	// view for the malformed query or fragment — the exact thing the bug
	// dropped silently — and must route the source range into unknown.
	t.Run("transform-failcloses-malformed-query", func(t *testing.T) {
		in := "http://h/?k=" + rawSpelling + "%2g"
		views, unknown := sourceMappedURIComponents(Identity(in), ProvLogRecord)
		if viewHas(views, ProvURIQueryPair, "k=ZZSECRETZZ%2g") {
			t.Fatalf("malformed query admitted a view (must fail-close): %+v", views)
		}
		lo := strings.Index(in, rawSpelling)
		if !covers(unknown, lo, lo+len(rawSpelling)+len("%2g")) {
			t.Fatalf("malformed query range not fail-closed: unknown=%d", len(unknown))
		}
	})
	t.Run("transform-failcloses-malformed-fragment", func(t *testing.T) {
		in := "http://h/p#" + rawSpelling + "%2g"
		views, unknown := sourceMappedURIComponents(Identity(in), ProvLogRecord)
		if viewHas(views, ProvURIComponent, "ZZSECRETZZ%2g") {
			t.Fatalf("malformed fragment admitted a view (must fail-close): %+v", views)
		}
		lo := strings.Index(in, rawSpelling)
		if !covers(unknown, lo, lo+len(rawSpelling)+len("%2g")) {
			t.Fatalf("malformed fragment range not fail-closed: unknown=%d", len(unknown))
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
