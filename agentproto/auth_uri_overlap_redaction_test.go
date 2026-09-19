package agentproto

import (
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
