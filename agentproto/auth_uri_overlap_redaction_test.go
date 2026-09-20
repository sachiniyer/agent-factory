package agentproto

import (
	"net/url"
	"strings"
	"testing"
)

// TestRedactAccessTokenURLRawQueryOverlapCrossProduct extends the existing
// cross-product guard (TestRedactAccessTokenURLRawQueryCrossProduct) to the
// adversarial %HH-over-literal shape: a valid percent escape (e.g. %ac) whose
// hex digits overlap the leading characters of the literal `access_token` in
// the raw bytes. The decode-based matcher collapses %ac to a single byte, so
// the decoded view the structured pass matches against loses the access_token=
// needle; without the raw-scan fallback in redactAccessTokenQueryPair, the
// pair re-emits verbatim from RawQuery and the redacted output still contains a
// literal access_token=<value> substring (#4187 regression).
//
// The cross-product structure mirrors the existing test's: every (separator ×
// value × overlap shape) cell asserts an exact-want redaction that preserves
// every neighbouring field byte-for-byte. The overlap shapes are:
//   - %ac   both hex digits ('a','c') steal two leading characters of
//     `access_token`; the raw pair literally contains "access_token="
//     starting at the 'a' immediately after '%'.
//   - %Aa   the second hex digit 'a' steals one leading character of
//     `access_token`; the raw pair literally contains "access_token="
//     starting at the 'a' that is the second hex digit.
//   - %AC   case-insensitive variant of %ac; the leading 'A' and 'C' fold to
//     'a' and 'c', so the raw pair still contains "access_token=".
func TestRedactAccessTokenURLRawQueryOverlapCrossProduct(t *testing.T) {
	keys := []struct {
		name string
		raw  string
	}{
		{name: "overlap lowercase hex ac", raw: "%access_token"},
		{name: "overlap mixed hex Aa", raw: "%Aaccess_token"},
		{name: "overlap uppercase hex AC", raw: "%ACcess_token"},
	}
	separators := []struct {
		name   string
		before string
		after  string
	}{
		{name: "ampersand", before: "before=1&", after: "&after=2"},
		{name: "semicolon", before: "before=1;", after: ";after=2"},
		{name: "none"},
	}
	values := []struct {
		name string
		raw  string
	}{
		{name: "present", raw: "TOKSECRET"},
		{name: "empty"},
	}

	for _, key := range keys {
		for _, separator := range separators {
			for _, value := range values {
				name := strings.Join([]string{key.name, separator.name, value.name}, "/")
				t.Run(name, func(t *testing.T) {
					prefix := "http://localhost:3000/?"
					input := prefix + separator.before + key.raw + "=" + value.raw + separator.after
					want := prefix + separator.before + key.raw + "=" + accessTokenRedaction + separator.after
					if got := RedactAccessTokenURL(input); got != want {
						t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
					}
				})
			}
		}
	}
}

// TestRedactAccessTokenURLRedactsOverlapValueNestedInQueryValue covers the case
// where the overlap access_token field rides inside the value of a different
// query pair (next=%access_token=...). The key-decode pass correctly skips it
// (the key is `next`), and the decoded scan fails because the overlapping %HH
// collapses access_token= out of the value's decoded view. The raw-pair scan
// catches it inside the nested value without over-redacting the prefix `next=`
// or the surrounding structure.
func TestRedactAccessTokenURLRedactsOverlapValueNestedInQueryValue(t *testing.T) {
	const input = "http://localhost:3000/?keep=hello&next=%access_token=TOKSECRET&after=2"
	const want = "http://localhost:3000/?keep=hello&next=%access_token=REDACTED&after=2"
	if got := RedactAccessTokenURL(input); got != want {
		t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
	}
}

// TestRedactAccessTokenURLRedactsOverlapComponent is the component-sweep mirror
// of the query overlap cross product above. The decoded sweep runs against
// u.Path / u.Fragment (the percent-DECODED fields), so the same %ac overlap that
// defeated the query decoder defeats it too: the decoded field loses the
// access_token= needle and u.RawPath / u.RawFragment carry the literal
// access_token=<value> into parsed.String() verbatim. The raw-bytes scan in
// redactAccessTokenComponents catches the overlap the parser-proven decoded
// field cannot.
//
// Each case uses a sentinel carrying the bug provenance so a future regression
// cannot accidentally satisfy an absent-token check by failing some other way.
func TestRedactAccessTokenURLRedactsOverlapComponent(t *testing.T) {
	const secret = "af-sentinel-overlap-component"
	cases := []struct {
		component string
		raw       string
		wantExact string // when set, the redacted URL must equal wantExact
	}{
		{
			component: "path",
			raw:       "http://h/path/%access_token=" + secret,
			wantExact: "http://h/path/%access_token=REDACTED",
		},
		{
			component: "path-uppercase-hex",
			raw:       "http://h/path/%Aaccess_token=" + secret,
			wantExact: "http://h/path/%Aaccess_token=REDACTED",
		},
		{
			component: "path-neighbour-kept",
			raw:       "http://h/path/%access_token=" + secret + "/suffix",
			wantExact: "http://h/path/%access_token=REDACTED/suffix",
		},
		{
			component: "fragment",
			raw:       "http://h/path#%access_token=" + secret,
			wantExact: "http://h/path#%access_token=REDACTED",
		},
		{
			component: "fragment-uppercase-hex",
			raw:       "http://h/path#%Aaccess_token=" + secret,
			wantExact: "http://h/path#%Aaccess_token=REDACTED",
		},
		{
			component: "opaque",
			raw:       "data:%access_token=" + secret,
			wantExact: "data:%access_token=REDACTED",
		},
		{
			component: "opaque-uppercase-hex",
			raw:       "data:%Aaccess_token=" + secret,
			wantExact: "data:%Aaccess_token=REDACTED",
		},
		{
			component: "opaque-neighbour-kept",
			raw:       "data:%access_token=" + secret + ";base64",
			wantExact: "data:%access_token=REDACTED;base64",
		},
	}
	for _, tc := range cases {
		t.Run(tc.component, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if strings.Contains(got, secret) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; secret %q survived",
					tc.raw, got, secret)
			}
			if !strings.Contains(got, accessTokenRedaction) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; want redaction marker",
					tc.raw, got)
			}
			if tc.wantExact != "" && got != tc.wantExact {
				t.Errorf("RedactAccessTokenURL(%q)\n  got  %q\n  want %q",
					tc.raw, got, tc.wantExact)
			}
		})
	}
}

// TestRedactAccessTokenURLKeepsOverlapFreeFormControls pins the fidelity side
// of the raw-scan fallback: an adversarial shape WITHOUT an access_token= value
// (the %HH shares its literal characters with a benign name) must NOT be
// rewritten just because the prefix happens to contain a valid percent
// escape. The decoded sweep already covers normal encoded-but-clean URLs; this
// test guards against an over-aggressive raw scan that would normalise every
// %hh-prefixed field whether or not it was an access_token parameter.
func TestRedactAccessTokenURLKeepsOverlapFreeFormControls(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"benign overlap query key", "http://h/?%acookie=ok"},
		{"benign overlap path segment", "http://h/p/%acookie=ok"},
		{"benign overlap fragment", "http://h/p#%acookie=ok"},
		{"benign overlap opaque", "data:%acookie=ok"},
		{"plain opaque untouched", "mailto:user%40host.example"},
		{"plain opaque slash space untouched", "af:a%2Fb%20c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactAccessTokenURL(tc.raw); got != tc.raw {
				t.Errorf("RedactAccessTokenURL(%q) = %q, want it returned unchanged: "+
					"no credential matched, the URL must keep its original escaping",
					tc.raw, got)
			}
		})
	}
}

// TestRedactAccessTokenURLOverlapCaseInsensitive pins that the raw scan is
// case-insensitive on the access_token= needle, mirroring the existing
// key-equality (strings.EqualFold) and decoded-needle (indexFoldASCII) checks.
// An attacker who controls a web-tab target can vary the token name's case to
// bypass a case-sensitive matcher; the redaction boundary must not depend on
// casing.
func TestRedactAccessTokenURLOverlapCaseInsensitive(t *testing.T) {
	const secret = "af-sentinel-overlap-case"
	for _, tc := range []struct {
		name, raw, wantExact string
	}{
		{"query lowercase needle", "http://h/?%Access_Token=" + secret, "http://h/?%Access_Token=REDACTED"},
		{"query uppercase field", "http://h/?%ACcess_token=" + secret, "http://h/?%ACcess_token=REDACTED"},
		{"path uppercase field", "http://h/p/%Access_Token=" + secret, "http://h/p/%Access_Token=REDACTED"},
		{"fragment uppercase field", "http://h/p#%ACCESS_TOKEN=" + secret, "http://h/p#%ACCESS_TOKEN=REDACTED"},
		{"opaque uppercase field", "data:%AcCeSs_ToKeN=" + secret, "data:%AcCeSs_ToKeN=REDACTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if strings.Contains(got, secret) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; secret %q survived",
					tc.raw, got, secret)
			}
			if tc.wantExact != "" && got != tc.wantExact {
				t.Errorf("RedactAccessTokenURL(%q)\n  got  %q\n  want %q",
					tc.raw, got, tc.wantExact)
			}
		})
	}
}

// TestRedactAccessTokenURLRawQueryOverlapAfterEqualToLiteralKey pins that the
// raw-scan fallback does not redact a neighbouring access_token field that the
// decoded scan already redacted (no double-redact / no value corruption), and
// that a string like "&access_token=LTOK&%access_token=RTOK" — where the first
// access_token= is the clean literal the structured pass handles and the
// second is the overlap that the raw scan handles — has both values redacted
// independently without one consuming the other.
func TestRedactAccessTokenURLRawQueryOverlapAfterEqualToLiteralKey(t *testing.T) {
	const input = "http://h/?access_token=LTOK&%access_token=RTOK"
	const want = "http://h/?access_token=REDACTED&%access_token=REDACTED"
	if got := RedactAccessTokenURL(input); got != want {
		t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
	}
}

// TestRedactAccessTokenURLRedactsOverlapAndLiteralInOneComponent pins the gap
// in #4663's overlap protection that the per-pair `if found { return }` (and
// the per-component path/fragment `else` gate) opened for the co-located
// shape: a single parser-proven component that carries both a %HH-overlapped
// access_token=<secret> (invisible to the decoded matcher, because the
// overlap's hex digits borrowed the leading 'a' and 'c' of the literal
// "access_token" word) and a later literal access_token=<value> (which the
// decoded matcher's single-anchor span selector matched, returning a
// tail-only span from the trailing literal). #4663's gate then
// short-circuited the raw-bytes scan for the entire component (the gate was
// meant to keep the raw scan from reprocessing already-redacted spans, but
// with the single-anchor decoded matcher it blocked the raw scan for the
// whole pair/component), so the overlap secret survived verbatim in the
// re-emitted URL.
//
// The fix runs the raw scan unconditionally after the decoded pass on the
// decoded-pass output, relying on idempotency — the decoded pass's redacted
// span is the literal REDACTED marker (no access_token= needle), so the raw
// scan can only match a genuine surviving access_token= overlap and never
// reprocesses a span the decoded pass already redacted — instead of a
// code-path gate, which collapsed when the decoded matcher's single-anchor
// span selector matched the trailing literal.
//
// Cases (mutated from #4663's existing overlap cross-product and component
// tests, all production-reachable; opaque is excluded because no in-repo
// caller passes an opaque URL through RedactAccessTokenURL):
//  1. Co-located overlap+literal in one component (query / path / fragment),
//     one case per %ac / %Aa / %AC hex variant — the trailing literal drives
//     the decoded pass; the un-gated raw scan then catches the overlap. For
//     path/fragment the decoded pass rewrites u.Path/u.Fragment and clears
//     u.RawPath/u.RawFragment, so EscapedPath/EscapedFragment re-encode the
//     overlap's decoded byte through the canonical uppercase %XX, and the
//     raw-scan output carries that canonical form regardless of the input
//     hex case. (Decoded byte 0xAC, from "%ac" or "%AC", encodes as "%AC";
//     byte 0xAA, from "%Aa", encodes as "%AA".)
//  2. Over-redaction guard cases — coincidental `access_token=` substrings in
//     non-access_token field values (path / fragment shapes), with a trailing
//     `/seg` segment in some cases. These have NO overlap; the decoded pass
//     already redacts a tail-from-the-coincidental-access_token= span, and the
//     raw scan re-runs over the REDACTED marker. The exact-want assertion pins
//     that the un-gated raw scan is idempotent (no extra redaction, no value
//     corruption, no escape normalisation) — the output byte-for-byte equals
//     what the gate would have produced.
//  3. Benign cases with no access_token key/value/overlap at all — the URL is
//     returned unchanged, pinning the #4161 no-normalization contract for
//     unmatched shapes.
//  4. Round-trip consistency — for every case, url.Parse(redacted).String()
//     == redacted. A production emitter that feeds the redacted URL back
//     through url.Parse (e.g. re-logging or re-routing) must see the same
//     string, with no normalisation drift from re-encoding the now-REDACTED
//     bytes.
func TestRedactAccessTokenURLRedactsOverlapAndLiteralInOneComponent(t *testing.T) {
	const overlapSecret = "af-sentinel-colocated-overlap-secret"
	const literalValue = "af-sentinel-colocated-literal-value"
	cases := []struct {
		name      string
		raw       string
		wantExact string
	}{
		// --- Co-located overlap + literal in one component (query family) ---
		// RawQuery stores the raw pair bytes verbatim through the decoded
		// pass, so the raw scan's output preserves the input hex case.
		{
			name:      "co-located/query/lower-hex",
			raw:       "http://h/?%access_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/?%access_token=REDACTED",
		},
		{
			name:      "co-located/query/mixed-hex",
			raw:       "http://h/?%Aaccess_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/?%Aaccess_token=REDACTED",
		},
		{
			name:      "co-located/query/upper-hex",
			raw:       "http://h/?%ACcess_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/?%ACcess_token=REDACTED",
		},
		// --- Co-located overlap + literal in one component (path family) ---
		// The decoded pass rewrites u.Path (anchored at the trailing literal)
		// and drops u.RawPath, so EscapedPath re-encodes the overlap's decoded
		// byte through the canonical uppercase %XX: byte 0xAC → "%AC" and
		// byte 0xAA → "%AA", regardless of the input hex case.
		{
			name:      "co-located/path/lower-hex",
			raw:       "http://h/p/%access_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/p/%ACcess_token=REDACTED",
		},
		{
			name:      "co-located/path/mixed-hex",
			raw:       "http://h/p/%Aaccess_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/p/%AAccess_token=REDACTED",
		},
		{
			name:      "co-located/path/upper-hex",
			raw:       "http://h/p/%ACcess_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/p/%ACcess_token=REDACTED",
		},
		// --- Co-located overlap + literal in one component (fragment family) ---
		// Mirrors the path family.
		{
			name:      "co-located/fragment/lower-hex",
			raw:       "http://h/p#%access_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/p#%ACcess_token=REDACTED",
		},
		{
			name:      "co-located/fragment/mixed-hex",
			raw:       "http://h/p#%Aaccess_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/p#%AAccess_token=REDACTED",
		},
		{
			name:      "co-located/fragment/upper-hex",
			raw:       "http://h/p#%ACcess_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/p#%ACcess_token=REDACTED",
		},
		// Over-redaction guards. Coincidental `access_token=` in a
		// non-access_token field value; the decoded pass already redacts the
		// tail-from-the-coincidental-access_token= span. The un-gated raw
		// scan runs over the REDACTED marker (no access_token= needle in it),
		// so the output is byte-for-byte what the gate would have produced.
		{
			name:      "over-redact/path/coincidental-before-suffix-segment",
			raw:       "http://h/p/key=access_token=INNOCENTval/seg",
			wantExact: "http://h/p/key=access_token=REDACTED",
		},
		{
			name:      "over-redact/path/coincidental-at-end",
			raw:       "http://h/p/seg/key=access_token=INNOCENTval",
			wantExact: "http://h/p/seg/key=access_token=REDACTED",
		},
		{
			name:      "over-redact/path/coincidental-and-trailing-literal",
			raw:       "http://h/p/key=access_token=INNOCENTvalaccess_token=REAL/seg",
			wantExact: "http://h/p/key=access_token=REDACTED",
		},
		{
			name:      "over-redact/fragment/coincidental-at-end",
			raw:       "http://h/p#key=access_token=INNOCENTval",
			wantExact: "http://h/p#key=access_token=REDACTED",
		},
		{
			name:      "over-redact/fragment/coincidental-and-trailing-literal",
			raw:       "http://h/p#key=access_token=INNOCENTvalaccess_token=REAL",
			wantExact: "http://h/p#key=access_token=REDACTED",
		},
		// Benign: no access_token key/value/overlap at all. The URL is
		// returned unchanged (#4161 no-normalization contract for unmatched
		// shapes).
		{
			name:      "benign/path/no-access-token",
			raw:       "http://h/p/seg/other=val/x",
			wantExact: "http://h/p/seg/other=val/x",
		},
		{
			name:      "benign/fragment/no-access-token",
			raw:       "http://h/p#seg/other=val",
			wantExact: "http://h/p#seg/other=val",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if strings.Contains(got, overlapSecret) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; overlap secret %q survived",
					tc.raw, got, overlapSecret)
			}
			if strings.Contains(got, literalValue) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; literal value %q survived",
					tc.raw, got, literalValue)
			}
			if got != tc.wantExact {
				t.Errorf("RedactAccessTokenURL(%q)\n  got  %q\n  want %q",
					tc.raw, got, tc.wantExact)
			}
			// Round-trip: the redacted URL must re-parse to the same string
			// so a downstream emitter that calls url.Parse on the result sees
			// no second redaction opportunity and no normalisation drift.
			if reparsed, err := url.Parse(got); err != nil {
				t.Errorf("url.Parse(%q) error = %v", got, err)
			} else if reparsed.String() != got {
				t.Errorf("round-trip url.Parse(%q).String() = %q; want %q",
					got, reparsed.String(), got)
			}
		})
	}
}

// TestRedactAccessTokenURLCoLocatedOverlapCaseInsensitive pins the case-
// insensitive behaviour of the raw overlap scan when the co-located shape
// puts both an overlap and a later literal in the same component, mirroring
// the existing single-overlap TestRedactAccessTokenURLOverlapCaseInsensitive.
// The raw scan is the only path that can see the overlap needle (the decoded
// matcher sees byte 0xAC + "cess_token=" rather than "access_token="), so its
// case-insensitivity is what an attacker who controls a web-tab target's URL
// cannot bypass by mixing the case of the overlap's "access_token=" word.
func TestRedactAccessTokenURLCoLocatedOverlapCaseInsensitive(t *testing.T) {
	const overlapSecret = "af-sentinel-colocated-overlap-case"
	const literalValue = "af-sentinel-colocated-overlap-literal"
	cases := []struct {
		name      string
		raw       string
		wantExact string
	}{
		{
			// ", A, c" overlap "Ac" consumes 'A' & 'c' of "Access_Token".
			name:      "query/mixed-case-needle",
			raw:       "http://h/?%Access_Token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/?%Access_Token=REDACTED",
		},
		{
			// Path: overlapped byte 0xAC re-encodes as "%AC", but the rest of
			// the overlap's word was uppercase-in-mixed-case, which u.Path
			// preserves and EscapedPath re-emits verbatim.
			name:      "path/mixed-case-needle",
			raw:       "http://h/p/%Access_Token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/p/%ACcess_Token=REDACTED",
		},
		{
			// Path: fully uppercase overlap word; the leading "%AC" decodes the
			// overlap to byte 0xAC, leaving "CESS_TOKEN=" as the rest of the
			// overlap's needle. EscapedPath re-encodes the byte as "%AC" and
			// the trailing literal is redacted along with the overlap's value
			// span.
			name:      "path/uppercase-needle",
			raw:       "http://h/p/%ACCESS_TOKEN=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/p/%ACCESS_TOKEN=REDACTED",
		},
		{
			// Fragment: fully uppercase overlap word; %AC then "CESS_TOKEN".
			name:      "fragment/uppercase-needle",
			raw:       "http://h/p#%ACCESS_TOKEN=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "http://h/p#%ACCESS_TOKEN=REDACTED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if strings.Contains(got, overlapSecret) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; overlap secret %q survived",
					tc.raw, got, overlapSecret)
			}
			if strings.Contains(got, literalValue) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; literal value %q survived",
					tc.raw, got, literalValue)
			}
			if !strings.Contains(got, accessTokenRedaction) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; want redaction marker",
					tc.raw, got)
			}
			if got != tc.wantExact {
				t.Errorf("RedactAccessTokenURL(%q)\n  got  %q\n  want %q",
					tc.raw, got, tc.wantExact)
			}
			if reparsed, err := url.Parse(got); err != nil {
				t.Errorf("url.Parse(%q) error = %v", got, err)
			} else if reparsed.String() != got {
				t.Errorf("round-trip url.Parse(%q).String() = %q; want %q",
					got, reparsed.String(), got)
			}
		})
	}
}
