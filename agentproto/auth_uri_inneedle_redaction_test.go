package agentproto

import (
	"strings"
	"testing"
)

// TestRedactAccessTokenURLRawQueryInNeedleCrossProduct is the in-needle-escape
// sibling of TestRedactAccessTokenURLRawQueryOverlapCrossProduct. The
// decode-based matcher misses a key whose leading 'a' an overlapping %HH
// collapses (decoded view loses the access_token= needle); the #4663
// raw-bytes fallback misses it again when a `%HH` *inside* the needle spells
// one of its characters (raw bytes carry "access%5Ftoken=", not the literal
// "access_token="). Only a raw-bytes matcher that tolerates `%HH` at any
// needle position catches the combined shape.
//
// The cross-product structure mirrors the existing overlap cross product: every
// (overlap shape × in-needle escape × separator × present/empty value) cell
// asserts an exact-want redaction that preserves the matched key form
// byte-for-byte and every neighbouring field too. The combination is the
// adversarial shape the bug report identifies; neither half alone defeats the
// existing two passes (the positive controls below pin both halves).
//
// Overlap shapes (the leading %HH that consumes the leading 'a'):
//   - %ac   both hex digits ('a','c') overlap the literal `access_token` word;
//     the literal tail after the overlap still spells `access_token` once the
//     in-needle escape is tolerated.
//
// In-needle escapes (the `%HH` that spells a needle character after the
// overlap):
//   - %5F  decodes to '_' (the unreserved char between `access` and `token`)
//   - %73  decodes to 's' (a mid-needle character, second nibble sanity check)
//   - %65  decodes to 'e' (a mid-needle character, second nibble sanity check)
//
// Each cell asserts exactWant: the matched key form (overlap `%` plus the
// `access<in-needle-escape>token=` literal-with-escape tail) is preserved
// verbatim, only the value is rewritten (#4161 fidelity contract, mirroring
// the overlap-only convention in TestRedactAccessTokenURLRawQueryOverlapCrossProduct).
func TestRedactAccessTokenURLRawQueryInNeedleCrossProduct(t *testing.T) {
	keys := []struct {
		name string
		raw  string
	}{
		{name: "overlap-ac-inneedle-5F", raw: "%access%5Ftoken"},
		{name: "overlap-ac-inneedle-73", raw: "%acces%73_token"},
		{name: "overlap-ac-inneedle-65", raw: "%acc%65ss_token"},
		{name: "overlap-AC-inneedle-5F-uppercase", raw: "%ACcess%5Ftoken"},
		{name: "overlap-A-inneedle-5f-mixed", raw: "%Aaccess%5ftoken"},
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

// TestRedactAccessTokenURLRedactsInNeedleValueNestedInQueryValue mirrors
// TestRedactAccessTokenURLRedactsOverlapValueNestedInQueryValue for the
// in-needle-escape family. The access_token field rides inside a different
// pair's value (next=%access%5Ftoken=...). The key-decode pass skips it (the
// key is `next`); the decoded scan misses it (the overlap collapses 'a'); the
// raw-bytes scan with in-needle tolerance catches it inside the value without
// over-redacting the surrounding structure.
func TestRedactAccessTokenURLRedactsInNeedleValueNestedInQueryValue(t *testing.T) {
	const input = "http://localhost:3000/?keep=hello&next=%access%5Ftoken=TOKSECRET&after=2"
	const want = "http://localhost:3000/?keep=hello&next=%access%5Ftoken=REDACTED&after=2"
	if got := RedactAccessTokenURL(input); got != want {
		t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
	}
}

// TestRedactAccessTokenURLRedactsInNeedleComponent is the component-sweep mirror
// of TestRedactAccessTokenURLRedactsOverlapComponent for the in-needle-escape
// family. The decoded sweep misses the same way (overlap ate the leading
// 'a'); the raw-bytes pass misses the same way too (the in-needle `%5F`
// breaks the literal "access_token=" substring in RawPath/RawFragment/Opaque).
// Only the percent-tolerant raw matcher redacts across path/fragment/opaque,
// keeping neighbouring fields and the matched key form verbatim.
//
// Each case uses a sentinel carrying the bug provenance so a future regression
// cannot accidentally satisfy an absent-token check by failing some other way.
// wantExact pins both halves: the secret is gone AND the matched key form
// (overlap `%` + in-needle `%5F/%73/%65`) is preserved, only the value is
// rewritten.
func TestRedactAccessTokenURLRedactsInNeedleComponent(t *testing.T) {
	cases := []struct {
		component string
		raw       string
		wantExact string
	}{
		// --- path: terminator set is /;?# so a value never crosses a segment ---
		{
			component: "path-inneedle-5F",
			raw:       "http://h/p/%access%5Ftoken=af-sentinel-inneedle-path",
			wantExact: "http://h/p/%access%5Ftoken=REDACTED",
		},
		{
			component: "path-inneedle-73",
			raw:       "http://h/p/%acces%73_token=af-sentinel-inneedle-needle-s",
			wantExact: "http://h/p/%acces%73_token=REDACTED",
		},
		{
			component: "path-inneedle-65",
			raw:       "http://h/p/%acc%65ss_token=af-sentinel-inneedle-needle-e",
			wantExact: "http://h/p/%acc%65ss_token=REDACTED",
		},
		{
			component: "path-neighbour-kept",
			raw:       "http://h/p/%access%5Ftoken=af-sentinel-inneedle-neighbour/suffix",
			wantExact: "http://h/p/%access%5Ftoken=REDACTED/suffix",
		},
		{
			component: "path-semicolon-terminator",
			raw:       "http://h/p/%access%5Ftoken=af-sentinel-inneedle-semi;x",
			wantExact: "http://h/p/%access%5Ftoken=REDACTED;x",
		},
		{
			component: "path-multiple-in-needle-escapes",
			raw:       "http://h/p/%acc%65ss%5Ftok%65n=af-sentinel-inneedle-many",
			wantExact: "http://h/p/%acc%65ss%5Ftok%65n=REDACTED",
		},
		// --- fragment: same terminator set, escaper-independent of the path ---
		{
			component: "fragment-inneedle-5F",
			raw:       "http://h/p#%access%5Ftoken=af-sentinel-inneedle-fragment",
			wantExact: "http://h/p#%access%5Ftoken=REDACTED",
		},
		{
			component: "fragment-inneedle-73",
			raw:       "http://h/p#%acces%73_token=af-sentinel-inneedle-frag-s",
			wantExact: "http://h/p#%acces%73_token=REDACTED",
		},
		{
			component: "fragment-neighbour-kept",
			raw:       "http://h/p#%access%5Ftoken=af-sentinel-inneedle-frag?suffix",
			wantExact: "http://h/p#%access%5Ftoken=REDACTED?suffix",
		},
		// --- opaque: u.Opaque is printed verbatim; same terminator guarantees ---
		{
			component: "opaque-inneedle-5F",
			raw:       "data:%access%5Ftoken=af-sentinel-inneedle-opaque",
			wantExact: "data:%access%5Ftoken=REDACTED",
		},
		{
			component: "opaque-inneedle-65",
			raw:       "data:%acc%65ss_token=af-sentinel-inneedle-opaque-e",
			wantExact: "data:%acc%65ss_token=REDACTED",
		},
		{
			component: "opaque-neighbour-kept",
			raw:       "data:%access%5Ftoken=af-sentinel-inneedle-opaque;base64",
			wantExact: "data:%access%5Ftoken=REDACTED;base64",
		},
	}
	for _, tc := range cases {
		t.Run(tc.component, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			// Sentinel must not survive — a future regression cannot satisfy an
			// absent-token check by failing some other way.
			if strings.Contains(got, "af-sentinel") {
				t.Errorf("RedactAccessTokenURL(%q) = %q; sentinel survived", tc.raw, got)
			}
			// Matched key form preserved + value rewritten. wantExact pins both
			// halves; an accomplishing-error redaction (canonicalising the key
			// to `access_token=`) would fail this equality.
			if got != tc.wantExact {
				t.Errorf("RedactAccessTokenURL(%q)\n  got  %q\n  want %q", tc.raw, got, tc.wantExact)
			}
		})
	}
}

// TestRedactAccessTokenURLInNeedleEncodesEqualsInKey verifies the
// percent-tolerant matcher accepts a `%3D` for the trailing `=` of the needle.
// The decoded sweep would handle this when the key has no overlap, but the
// leading-overlap + encoded-`=` combination defeats the existing two passes
// the same way an in-needle `%5F` does.
func TestRedactAccessTokenURLInNeedleEncodesEqualsInKey(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantExact string
	}{
		{
			name:      "query-encoded-equals",
			raw:       "http://h/?%access%5Ftoken%3DSHECRET&view=2",
			wantExact: "http://h/?%access%5Ftoken%3DREDACTED&view=2",
		},
		{
			name:      "path-encoded-equals",
			raw:       "http://h/p/%access%5Ftoken%3Daf-sentinel-encoded-eq",
			wantExact: "http://h/p/%access%5Ftoken%3DREDACTED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if got != tc.wantExact {
				t.Errorf("RedactAccessTokenURL(%q)\n  got  %q\n  want %q", tc.raw, got, tc.wantExact)
			}
		})
	}
}

// TestRedactAccessTokenURLKeepsInNeedleFreeFormControls is the fidelity guard
// for the percent-tolerant raw matcher, mirroring
// TestRedactAccessTokenURLKeepsOverlapFreeFormControls. An adversarial shape
// that decodes to a non-access_token parameter name (even with a leading
// overlap AND an in-needle `%5F`) must NOT be rewritten just because the raw
// bytes happen to contain a percent escape. The decoded sweep already covers
// normal encoded-but-clean URLs; this test guards against an over-aggressive
// matcher that canonicalises every percent-prefixed field it can spell.
func TestRedactAccessTokenURLKeepsInNeedleFreeFormControls(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		// Overlap + in-needle `%5F` but NOT `access_token` — the decoded key is
		// a benign name; nothing should be redacted.
		{"benign inneedle query key", "http://h/?%acookie%5Fkind=ok"},
		{"benign inneedle path segment", "http://h/p/%acookie%5Fkind=ok"},
		{"benign inneedle fragment", "http://h/p#%acookie%5Fkind=ok"},
		{"benign inneedle opaque", "data:%acookie%5Fkind=ok"},
		// Overlap + an in-needle escape that decodes to a real needle char, but
		// the decoded key is `acook_e` — still not `access_token`.
		{"benign inneedle mid-char query key", "http://h/?%acook%65=ok"},
		// The unencoded spelling, with the benign name, untouched as a baseline.
		{"plain overlap-only benign", "http://h/?%acookie=ok"},
		// Plain opaque untouched — pins the #4161 pass-through for opaque that
		// carries no credential at all.
		{"plain opaque at-sign survives", "mailto:user%40host.example"},
		{"plain opaque slash space survives", "af:a%2Fb%20c"},
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

// TestRedactAccessTokenURLInNeedleCaseInsensitive pins that the percent-tolerant
// matcher is case-insensitive on its needle, mirroring both the existing
// key-equality (strings.EqualFold) check and
// TestRedactAccessTokenURLOverlapCaseInsensitive for the overlap-only family.
// Both the literal needle bytes AND the hex digits in a tolerated %HH may be
// any case; the redaction boundary must not depend on casing.
func TestRedactAccessTokenURLInNeedleCaseInsensitive(t *testing.T) {
	const secret = "af-sentinel-inneedle-case"
	for _, tc := range []struct {
		name, raw, wantExact string
	}{
		{"query upper hex hex", "http://h/?%ACcess%5FTOKEN=" + secret, "http://h/?%ACcess%5FTOKEN=REDACTED"},
		{"query lower hex hex", "http://h/?%access%5ftoken=" + secret, "http://h/?%access%5ftoken=REDACTED"},
		{"query mixed-case needle", "http://h/?%AcCeSs%5FtOkEn=" + secret, "http://h/?%AcCeSs%5FtOkEn=REDACTED"},
		{"path uppercase field", "http://h/p/%Access%5FToken=" + secret, "http://h/p/%Access%5FToken=REDACTED"},
		{"fragment uppercase field", "http://h/p#%ACCESS%5FTOKEN=" + secret, "http://h/p#%ACCESS%5FTOKEN=REDACTED"},
		{"opaque mixed-case field", "data:%AcCeSs%5FtOkEn=" + secret, "data:%AcCeSs%5FtOkEn=REDACTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if strings.Contains(got, secret) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; secret survived", tc.raw, got)
			}
			if tc.wantExact != "" && got != tc.wantExact {
				t.Errorf("RedactAccessTokenURL(%q)\n  got  %q\n  want %q",
					tc.raw, got, tc.wantExact)
			}
		})
	}
}

// TestRedactAccessTokenURLRawQueryInNeedleAfterEqualToLiteralKey pins that the
// percent-tolerant matcher redacts a neighbouring in-needle-escape field
// independently of a clean literal `access_token=` field the structured pass
// already redacted: no double-redact, no value corruption, no value-span cross
// talk. The first access_token= is the clean literal the structured pass
// handles; the second is the adversarial overlap+in-needle shape the raw scan
// handles — both values redact independently without one consuming the other,
// mirroring TestRedactAccessTokenURLRawQueryOverlapAfterEqualToLiteralKey for
// the overlap-only family.
func TestRedactAccessTokenURLRawQueryInNeedleAfterEqualToLiteralKey(t *testing.T) {
	const input = "http://h/?access_token=LTOK&%access%5Ftoken=RTOK"
	const want = "http://h/?access_token=REDACTED&%access%5Ftoken=REDACTED"
	if got := RedactAccessTokenURL(input); got != want {
		t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
	}
}

// TestRedactAccessTokenURLInNeedleEveryNibble sweeps every hex nibble as the
// in-needle escape for `_`, mirroring the bug report's claim that the
// bypass is not nibble-specific. Any nibble pair that decodes to a needle
// character defeats the literal-substring raw scan by the same mechanism;
// the percent-tolerant matcher handles them all uniformly.
func TestRedactAccessTokenURLInNeedleEveryNibble(t *testing.T) {
	// Each needle char and one valid %HH for it. 'a' is not used (it is the
	// overlap character and never escapes inside the tail); '_'/'s'/'e'/'o'/'k'
	// cover the unreserved-word chars an adversarial encoder could pick.
	for _, tc := range []struct {
		name    string
		keyTail string // raw key (with overlap and in-needle escape), after the leading %
		wantKey string // expected preserved key form (after the leading %)
	}{
		{"underscore-%5F", "access%5Ftoken=", "access%5Ftoken="},
		{"s-%73", "acces%73_token=", "acces%73_token="},
		{"e-%65", "acc%65ss_token=", "acc%65ss_token="},
		{"o-%6F", "access_t%6Fken=", "access_t%6Fken="},
		{"k-%6B", "access_to%6Ben=", "access_to%6Ben="},
		{"n-%6E", "access_toke%6E=", "access_toke%6E="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "http://h/?%" + tc.keyTail + "af-sentinel-nibble"
			want := "http://h/?%" + tc.wantKey + accessTokenRedaction
			if got := RedactAccessTokenURL(input); got != want {
				t.Errorf("RedactAccessTokenURL(%q)\n  got  %q\n  want %q", input, got, want)
			}
		})
	}
}

// TestRedactAccessTokenURLInNeedleMalformedEscapeAbort pins the malformed-%
// abort behaviour from the bug report: a `%` without two hex digits following
// cannot constitute an escape, so a candidate that would need to treat it as
// one aborts rather than silently treating it as a literal. The leading `%ac`
// overlap family relied on this fail-open-on-reserved behaviour during the
// #4663 fix; the percent-tolerant matcher preserves it. A key whose in-needle
// `%` is malformed is NOT redacted by the raw matcher; if the decoded sweep
// also missed it (overlap present), the URL passes through unchanged — a
// contract violation the bug report's "Recommended fix" explicitly notes is
// preserved (a malformed in-needle escape is not this bug's scope; it is the
// `%ac`-family precedent extended).
func TestRedactAccessTokenURLInNeedleMalformedEscapeAbort(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		// In-needle `%` followed by only one hex digit: not a valid escape, so
		// the matcher aborts; the decoded sweep also missed (overlap ate 'a').
		// The URL passes through unchanged — a known contract gap, mirroring
		// the %ac family's documented behaviour.
		{"in-needle single-hex-digit", "http://h/?%access%5token=SHECRET"},
		// In-needle `%` followed by a non-hex byte: same abort.
		{"in-needle non-hex", "http://h/?%access%zztoken=SHECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The fix is scoped to valid %HH escapes; the malformed cases pass
			// through unchanged by design (the bug report's Recommended fix:
			// "On a malformed % (no two hex digits following) the match aborts
			// at that position (no redaction)").
			got := RedactAccessTokenURL(tc.raw)
			if got != tc.raw {
				t.Errorf("RedactAccessTokenURL(%q) = %q, want it returned unchanged: "+
					"a malformed in-needle escape is not a valid overlap/escape shape "+
					"and must not be matched as one", tc.raw, got)
			}
		})
	}
}

// TestRedactAccessTokenURLInNeedleNestedEscapes pins the percent-tolerant raw
// matcher's extension to NESTED in-needle percent escapes — sequences whose
// outer %HH decodes to '%' and the next two raw bytes are its deeper hex
// pair, the same reducing-stack mechanism redactx.PercentDecode uses (e.g.
// %255F → %25→'%' + 5F → '_', %253D → %25→'%' + 3D → '='). A single-level raw
// matcher leaves these URLs verbatim: the decoded sweep still misses
// (leading-overlap %ac collapses the 'a'), and the raw bytes carry the byte
// '%' rather than the in-needle character, so the same credential leak this
// change is meant to close would persist. The reviewer's exact example URL
// `http://h/?%access%255Ftoken=SECRET` is the headline case; the rest pin the
// analogous nested-equals shape, certainty that arbitrary-depth nesting is
// handled in linear work, and a path/fragment/opaque component sweep. The
// matched key form is preserved byte-for-byte — only the value is rewritten
// (#4161 fidelity contract, mirroring the single-level in-needle family).
func TestRedactAccessTokenURLInNeedleNestedEscapes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		wantExact string
	}{
		// The reviewer's exact example URL (overlap + nested-encoded '_').
		{"query nested underscore", "http://h/?%access%255Ftoken=af-sentinel-nested-underscore", "http://h/?%access%255Ftoken=REDACTED"},
		// Nested-encoded '=' (the trailing separator encoded twice through %25).
		{"query nested equals", "http://h/?%access%5Ftoken%253Daf-sentinel-nested-equals", "http://h/?%access%5Ftoken%253DREDACTED"},
		// Both needle characters nested: %25 inside the underscore AND the separator.
		{"query nested underscore and equals", "http://h/?%access%255Ftoken%253Daf-sentinel-nested-underscore-equals", "http://h/?%access%255Ftoken%253DREDACTED"},
		// Triple-nested: %25255F → %255F → %5F → '_'. Confirms arbitrarily deep
		// nesting resolves in one pass and the matcher does not short-circuit
		// abort at an intermediate %25.
		{"query triple-nested underscore", "http://h/?%access%25255Ftoken=af-sentinel-triple-nested", "http://h/?%access%25255Ftoken=REDACTED"},
		// --- component sweep mirroring TestRedactAccessTokenURLRedactsInNeedleComponent
		// path: terminator set /;?# so a neighbour segment is kept verbatim.
		{"path nested underscore neighbour kept", "http://h/p/%access%255Ftoken=af-sentinel-nested-path/neighbour", "http://h/p/%access%255Ftoken=REDACTED/neighbour"},
		{"path nested semicolon terminator", "http://h/p/%access%255Ftoken=af-sentinel-nested-path;x", "http://h/p/%access%255Ftoken=REDACTED;x"},
		{"fragment nested underscore", "http://h/p#%access%255Ftoken=af-sentinel-nested-fragment", "http://h/p#%access%255Ftoken=REDACTED"},
		// opaque: terminator set is /;?# too; a trailing ;base64 keeps its place.
		{"opaque nested underscore", "data:%access%255Ftoken=af-sentinel-nested-opaque;base64", "data:%access%255Ftoken=REDACTED;base64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if strings.Contains(got, "af-sentinel") {
				t.Errorf("RedactAccessTokenURL(%q) = %q; sentinel survived", tc.raw, got)
			}
			if got != tc.wantExact {
				t.Errorf("RedactAccessTokenURL(%q)\n  got  %q\n  want %q", tc.raw, got, tc.wantExact)
			}
		})
	}
}
