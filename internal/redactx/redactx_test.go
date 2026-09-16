package redactx

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/redactspan"
)

// The decoder's reducing stack must resolve escapes exposed by earlier
// decoding without rescanning the input, so arbitrarily nested encoding stays
// linear-work. 4096 levels is the case that used to live in agentproto.
func TestPercentDecodeHandlesDeepNesting(t *testing.T) {
	const depth = 4096
	raw := "%" + strings.Repeat("25", depth-1) + "61ccess_token"
	view, malformed := PercentDecode(raw, false)
	if malformed {
		t.Fatal("PercentDecode(deep key) reported a malformed raw escape")
	}
	if got, want := view.Text, "access_token"; got != want {
		t.Fatalf("PercentDecode(deep key) = %q, want %q", got, want)
	}
	if got := view.Source[0]; got.Start != 0 || got.End != 1+2*depth {
		t.Fatalf("deeply decoded byte source = [%d,%d), want [0,%d)",
			got.Start, got.End, 1+2*depth)
	}
}

func TestPercentDecodeNestedAndMapping(t *testing.T) {
	// %2574 → %74 → 't'; the decoded byte's source range must cover the whole
	// outermost spelling so a match projects back over every layer.
	view, malformed := PercentDecode("x%2574y", false)
	if malformed {
		t.Fatal("unexpected malformed report")
	}
	if view.Text != "xty" {
		t.Fatalf("PercentDecode nested = %q, want %q", view.Text, "xty")
	}
	if got := view.Source[1]; got.Start != 1 || got.End != 6 {
		t.Fatalf("nested byte source = [%d,%d), want [1,6)", got.Start, got.End)
	}
	r, ok := view.MapSpan(1, 2)
	if !ok || r.Start != 1 || r.End != 6 {
		t.Fatalf("MapSpan = %v,%v want [1,6)", r, ok)
	}
}

func TestPercentDecodePlusIsOuterGrammarOnly(t *testing.T) {
	// '+' is space in the outer form grammar; %2B is a literal plus that must
	// not be re-interpreted at the next depth.
	view, _ := PercentDecode("a+b%2Bc", true)
	if view.Text != "a b+c" {
		t.Fatalf("plus handling = %q, want %q", view.Text, "a b+c")
	}
	view, _ = PercentDecode("a+b", false)
	if view.Text != "a+b" {
		t.Fatalf("plusAsSpace=false = %q, want %q", view.Text, "a+b")
	}
}

func TestPercentDecodeMalformedRaw(t *testing.T) {
	// A '%' not followed by two hex digits is ordinary data in the stable
	// view but marks the raw representation malformed.
	view, malformed := PercentDecode("a%2gb", false)
	if !malformed {
		t.Fatal("malformed raw escape not reported")
	}
	if view.Text != "a%2gb" {
		t.Fatalf("malformed view = %q, want %q", view.Text, "a%2gb")
	}
}

func TestViewMapSpanBounds(t *testing.T) {
	v := Identity("abc")
	if _, ok := v.MapSpan(0, 0); ok {
		t.Fatal("empty span mapped")
	}
	if _, ok := v.MapSpan(-1, 2); ok {
		t.Fatal("negative span mapped")
	}
	if _, ok := v.MapSpan(2, 4); ok {
		t.Fatal("out-of-bounds span mapped")
	}
	r, ok := v.MapSpan(1, 3)
	if !ok || r.Start != 1 || r.End != 3 {
		t.Fatalf("identity MapSpan = %v,%v", r, ok)
	}
}

func TestGoQuotedEnd(t *testing.T) {
	if got := GoQuotedEnd(`"a\"b" rest`, 0); got != 6 {
		t.Fatalf("GoQuotedEnd escaped quote = %d, want 6", got)
	}
	if got := GoQuotedEnd("\"a\nb\"", 0); got != -1 {
		t.Fatalf("GoQuotedEnd newline-terminated = %d, want -1", got)
	}
	if got := GoQuotedEnd(`"unclosed`, 0); got != -1 {
		t.Fatalf("GoQuotedEnd unclosed = %d, want -1", got)
	}
}

func TestEngineFailClosedOnUnparseableShell(t *testing.T) {
	// A proven shell command the POSIX parser cannot establish is an unknown
	// logical value, not text to match flat — the whole command is redacted.
	e := &Engine{
		Produce:    func(string, Provenance) []redactspan.Span { return nil },
		FailClosed: redactspan.Span{Replacement: "[x]", Priority: 0},
		Fallback:   "[x]",
	}
	// The %q-carried emitter form: the closed token decodes to a command the
	// shell parser rejects, so the whole token is rewritten to a redacted
	// re-encoding.
	got := e.Scrub(`post-worktree hook "echo 'unclosed"`, ProvLogRecord)
	if !strings.Contains(got, `"[x]"`) {
		t.Fatalf("unparseable emitter command not failed closed: %q", got)
	}
	// The legacy raw-field form: the command runs to the end of the record and
	// the unparseable region takes the marker directly.
	got = e.Scrub("running post-worktree hook in /tmp/w (output: /tmp/h.log): echo 'unclosed", ProvLogRecord)
	if !strings.Contains(got, ": [x]") {
		t.Fatalf("unparseable raw command not failed closed: %q", got)
	}
}
