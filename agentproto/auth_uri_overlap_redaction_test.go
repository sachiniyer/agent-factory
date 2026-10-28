package agentproto

import (
	"errors"
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
		// Host overlap rows (#4663 closed this leak for every other component
		// but left the host on the literal-only backstop). url.Parse decodes
		// the %HH overlap into u.Host (e.g. %ac → byte 0xAC, consuming the
		// 'a' and 'c' of `access`), so the literal-needle backstop cannot see
		// access_token=. The percent-tolerant scanner runs over the re-encoded
		// host (url.URL.String re-escapes the decoded byte), which has no
		// Raw* twin to preserve the input hex case the way EscapedPath does, so
		// the overlap byte always re-encodes through the canonical uppercase
		// %XX: %ac/%AC → "%AC", %Aa → "%AA". The port after the single ':'
		// host/port separator is kept, mirroring the userinfo ':' terminator.
		{
			component: "host",
			raw:       "http://%access_token=" + secret + ":8443/stream",
			wantExact: "http://%ACcess_token=REDACTED:8443/stream",
		},
		{
			component: "host-mixed-hex",
			raw:       "http://%Aaccess_token=" + secret + ":8443/stream",
			wantExact: "http://%AAccess_token=REDACTED:8443/stream",
		},
		{
			component: "host-upper-overlap",
			raw:       "http://%ACcess_token=" + secret + ":8443/stream",
			wantExact: "http://%ACcess_token=REDACTED:8443/stream",
		},
		{
			component: "host-port-kept",
			raw:       "http://%access_token=" + secret + ":8443/path",
			wantExact: "http://%ACcess_token=REDACTED:8443/path",
		},
		{
			component: "host-no-port",
			raw:       "http://%access_token=" + secret + "/path",
			wantExact: "http://%ACcess_token=REDACTED/path",
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
		// Host: the overlap byte re-encodes through the canonical uppercase %XX
		// (host has no Raw* twin), so the canonical "%AC" prefix collapses to
		// "%AC" and the trailing word keeps its input case.
		{"host uppercase field", "http://%ACcess_token=" + secret + ":8443/", "http://%ACcess_token=REDACTED:8443/"},
		{"host mixed-case field", "http://%Access_Token=" + secret + ":8443/", "http://%ACcess_Token=REDACTED:8443/"},
		{"host full uppercase", "http://%ACCESS_TOKEN=" + secret + ":8443/", "http://%ACCESS_TOKEN=REDACTED:8443/"},
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
// tests, all production-reachable; opaque is included because the function's
// component-wide contract — and the existing opaque-URL coverage in
// TestRedactAccessTokenURLRedactsOverlapComponent /
// TestRedactAccessTokenURLKeepsOverlapFreeFormControls /
// TestRedactAccessTokenURLOverlapCaseInsensitive — applies to opaque too, even
// though no current in-repo production caller happens to route an opaque URL
// through it; userinfo is included because the same component-wide contract
// applies to it too — url.URL.User is a parser-proven field that url.URL.String
// re-emits through Userinfo.String, so a co-located %HH-overlapped
// access_token=<secret> plus a later literal access_token=<value> in the
// username or password leaks through the same decoded-single-anchor path the
// other components did, and the raw scan mirrors it):
//  1. Co-located overlap+literal in one component (query / path / fragment /
//     opaque / userinfo), one case per %ac / %Aa / %AC hex variant — the
//     trailing literal drives the decoded pass; the un-gated raw scan then
//     catches the overlap. For path/fragment the decoded pass rewrites
//     u.Path/u.Fragment and clears u.RawPath/u.RawFragment, so
//     EscapedPath/EscapedFragment re-encode the overlap's decoded byte
//     through the canonical uppercase %XX, and the raw-scan output carries
//     that canonical form regardless of the input hex case. (Decoded byte
//     0xAC, from "%ac" or "%AC", encodes as "%AC"; byte 0xAA, from "%Aa",
//     encodes as "%AA".) Opaque carries no Raw* twin: url.URL.String prints
//     u.Opaque verbatim, so its raw-scan output preserves the input hex case
//     byte-for-byte. Userinfo stores the DECODED name/password (url.Parse
//     already percent-decoded them) and Userinfo.String re-encodes through
//     encodeUserinfo, so userinfo mirrors the path family's canonical
//     uppercase %XX re-encode. The password sub-variant keeps the username
//     untouched and parks the overlap in the password to also exercise the
//     single ":" user/password separator — the only structural terminator in
//     Userinfo.String's grammar (a literal ":" in either field is escaped as
//     %3A, so the first ":" in the serialized form is always the
//     separator): an access_token= span in the name ends at ":" (or at the
//     end of the string when there is no password), and one in the password
//     — the trailing field — runs to the end of the string.
//  2. Over-redaction guard cases — coincidental `access_token=` substrings in
//     non-access_token field values (path / fragment / opaque / userinfo
//     shapes), with a trailing `/seg` segment in some cases. These have NO
//     overlap; the decoded pass already redacts a tail-from-the-coincidental-
//     access_token= span, and the raw scan re-runs over the REDACTED marker.
//     The exact-want assertion pins that the un-gated raw scan is idempotent
//     (no extra redaction, no value corruption, no escape normalisation) —
//     the output byte-for-byte equals what the gate would have produced.
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
		// --- Co-located overlap + literal in one component (opaque family) ---
		// Opaque carries no Raw* twin: url.URL.String prints u.Opaque verbatim,
		// so the raw-scan output preserves the overlap's input hex case
		// byte-for-byte (no Escaped*-style canonical re-encode, unlike
		// path/fragment).
		{
			name:      "co-located/opaque/lower-hex",
			raw:       "data:%access_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "data:%access_token=REDACTED",
		},
		{
			name:      "co-located/opaque/mixed-hex",
			raw:       "data:%Aaccess_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "data:%Aaccess_token=REDACTED",
		},
		{
			name:      "co-located/opaque/upper-hex",
			raw:       "data:%ACcess_token=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "data:%ACcess_token=REDACTED",
		},
		// --- Co-located overlap + literal in one component (userinfo family) ---
		// Userinfo stores the DECODED name/password (url.Parse already
		// percent-decoded them) and Userinfo.String re-encodes them through
		// encodeUserinfo, so the userinfo family mirrors the path family
		// above: the overlap's decoded byte (0xAC from "%ac" / "%AC", 0xAA
		// from "%Aa") re-encodes through the canonical uppercase %XX. The
		// password sub-variant keeps the username untouched and parks the
		// overlap in the password to also exercise the single ":"
		// user/password separator — the only structural terminator in
		// Userinfo.String's grammar (a literal ":" in either field is
		// escaped as %3A, so the first ":" in the serialized form is always
		// the separator): the overlap's value runs to the end of the
		// serialized string, since the password is the trailing field.
		{
			name:      "co-located/userinfo/lower-hex",
			raw:       "http://%access_token=" + overlapSecret + "access_token=" + literalValue + "@h/",
			wantExact: "http://%ACcess_token=REDACTED@h/",
		},
		{
			name:      "co-located/userinfo/mixed-hex",
			raw:       "http://%Aaccess_token=" + overlapSecret + "access_token=" + literalValue + "@h/",
			wantExact: "http://%AAccess_token=REDACTED@h/",
		},
		{
			name:      "co-located/userinfo/upper-hex",
			raw:       "http://%ACcess_token=" + overlapSecret + "access_token=" + literalValue + "@h/",
			wantExact: "http://%ACcess_token=REDACTED@h/",
		},
		{
			name:      "co-located/userinfo-password/lower-hex",
			raw:       "http://user:%access_token=" + overlapSecret + "access_token=" + literalValue + "@h/",
			wantExact: "http://user:%ACcess_token=REDACTED@h/",
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
		{
			name:      "over-redact/opaque/coincidental-at-end",
			raw:       "data:key=access_token=INNOCENTval",
			wantExact: "data:key=access_token=REDACTED",
		},
		{
			name:      "over-redact/opaque/coincidental-and-trailing-literal",
			raw:       "data:%access_token=INNOCENTvalaccess_token=REAL;base64",
			wantExact: "data:%access_token=REDACTED",
		},
		// Userinfo over-redact guards. The decoded pass already redacts the
		// tail-from-the-coincidental-access_token= span (extending to the end
		// of the username when there is no password, or to the end of the
		// serialized userinfo in the password case, since the password is
		// the trailing field). The un-gated raw scan re-runs over the
		// REDACTED marker, so the output byte-for-byte matches what a gate
		// would have produced.
		{
			name:      "over-redact/userinfo/coincidental-at-end",
			raw:       "http://key=access_token=INNOCENTval@h/",
			wantExact: "http://key=access_token=REDACTED@h/",
		},
		{
			name:      "over-redact/userinfo-password/coincidental-and-trailing-literal",
			raw:       "http://user:key=access_token=INNOCENTvalaccess_token=REAL@h/",
			wantExact: "http://user:key=access_token=REDACTED@h/",
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
		{
			name:      "benign/opaque/no-access-token",
			raw:       "data:seg/other=val/x",
			wantExact: "data:seg/other=val/x",
		},
		{
			name:      "benign/userinfo/no-access-token",
			raw:       "http://key=val@h/x",
			wantExact: "http://key=val@h/x",
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
		{
			// Opaque: fully uppercase overlap word. Opaque carries no Raw*
			// twin, so url.URL.String prints u.Opaque verbatim and the
			// input hex case is preserved byte-for-byte in the redacted
			// output (no Escaped*-style canonical re-encode, unlike the
			// path family above).
			name:      "opaque/uppercase-needle",
			raw:       "data:%ACCESS_TOKEN=" + overlapSecret + "access_token=" + literalValue,
			wantExact: "data:%ACCESS_TOKEN=REDACTED",
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

// TestRedactAccessTokenURLRedactsOverlapHost is the focused regression guard for
// the host branch gap left by #4663: every other URL component was routed
// through the percent-tolerant scanner (redactRawAccessTokenValue), but the
// host kept only the literal-needle backstop (RedactAccessTokenText). A %HH
// overlap in the authority (e.g. %ac → byte 0xAC, consuming the leading 'a' and
// 'c' of `access`) collapses access_token= out of the DECODED u.Host that the
// backstop scans, while url.URL.String re-escapes 0xAC back to %AC and emits a
// literal access_token=<value> in the re-serialized URL — leaking the secret
// into the dial-error chain (#4663 closed this for path/fragment/opaque/
// userinfo; the host was the sole unprotected component).
//
// url.Parse tolerates the high-byte overlap (%ac → 0xAC) in the host (unlike
// %61/%a1 which decode to letters and turn the host into a literal
// access_token= that url.Parse then rejects over '='), so the overlap survives
// the parse and reaches the redactor. The host has no Raw* twin (EscapedPath/
// EscapedFragment preserve the input hex case via RawPath/RawFragment), so
// String() always re-encodes the overlap's decoded byte through the canonical
// uppercase %XX: %ac/%AC → "%AC", %Aa → "%AA". ":" is the host's only structural
// separator (it precedes the port), so a redacted value ends at it — keeping
// the port in the diagnostic, mirroring the userinfo ':' terminator. Each case
// round-trips so a downstream emitter that re-parses the redacted URL sees the
// same string (no normalisation drift from re-encoding the REDACTED bytes).
//
// The nested in-needle case (%255F → '%' + '5F' → '_' once the raw matcher's
// reducing stack resolves it) is included to pin that the host branch inherits
// the in-needle family too, not just the overlap-only shape; the single-level
// %5F in-needle shape is rejected by url.Parse itself (pre-existing fail
// closed) and is omitted from this table.
func TestRedactAccessTokenURLRedactsOverlapHost(t *testing.T) {
	const secret = "af-sentinel-overlap-host"
	cases := []struct {
		name      string
		raw       string
		wantExact string
	}{
		// Overlap hex variants; the re-encoded prefix always canonicalises to
		// the uppercase %XX of the decoded byte.
		{"overlap-lower-hex", "http://%access_token=" + secret + ":8443/stream", "http://%ACcess_token=REDACTED:8443/stream"},
		{"overlap-mixed-hex", "http://%Aaccess_token=" + secret + ":8443/stream", "http://%AAccess_token=REDACTED:8443/stream"},
		{"overlap-upper-hex", "http://%ACcess_token=" + secret + ":8443/stream", "http://%ACcess_token=REDACTED:8443/stream"},
		// Port kept (the host's ':' separator is the value terminator).
		{"overlap-port-kept", "http://%access_token=" + secret + ":8443/path", "http://%ACcess_token=REDACTED:8443/path"},
		// No port: the value runs to the end of the host.
		{"overlap-no-port", "http://%access_token=" + secret + "/path", "http://%ACcess_token=REDACTED/path"},
		// Protocol-relative URL (empty scheme): the prefix is "//" not
		// "<scheme>://", which the host extraction must handle without dropping
		// the first host byte.
		{"overlap-empty-scheme", "//%access_token=" + secret + ":8443/stream", "//%ACcess_token=REDACTED:8443/stream"},
		// ws: the production dial scheme (parseDaemonURL → websocket.Dial).
		{"overlap-ws-scheme", "ws://%access_token=" + secret + ":8443/stream", "ws://%ACcess_token=REDACTED:8443/stream"},
		// Nested in-needle escape in the host (%255F → '_'). pins the host
		// branch carries the in-needle family, not just the overlap shape.
		{"overlap-nested-inneedle", "http://%access%255Ftoken=" + secret + ":8443/", "http://%ACcess%255Ftoken=REDACTED:8443/"},
		// Literal control: the backstop already caught this; the fix must keep
		// redacting it (and now preserves the port the backstop used to drop).
		{"literal-with-port", "http://access_token=" + secret + ":8443/stream", "http://access_token=REDACTED:8443/stream"},
		{"literal-no-port", "http://access_token=" + secret + "/stream", "http://access_token=REDACTED/stream"},
		// Colon INSIDE the value: url.Parse splits the authority at the LAST
		// colon whose suffix is a port, so ...=TOP:SECRET:8443 parses with
		// hostname ...=TOP:SECRET and port 8443. A ':' value terminator would
		// truncate at the first colon (TOP) and leak SECRET; the port is
		// stripped first and re-appended, so the value runs through its inner
		// ':' to the end of the host segment and the trailing :8443 survives.
		{"overlap-colon-in-value", "http://%access_token=" + secret + ":SECRET:8443/stream", "http://%ACcess_token=REDACTED:8443/stream"},
		{"literal-colon-in-value", "http://access_token=" + secret + ":SECRET:8443/stream", "http://access_token=REDACTED:8443/stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if strings.Contains(got, secret) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; secret %q survived",
					tc.raw, got, secret)
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

// TestRedactAccessTokenURLKeepsOverlapHostFreeForm is the host sibling of
// TestRedactAccessTokenURLKeepsOverlapFreeFormControls. A host carrying a %HH
// overlap that does NOT spell access_token (e.g. %acookie=ok) must NOT gain a
// redaction marker just because the raw scan runs over the re-encoded host.
// Unlike the path/fragment/opaque free-form rows this test is NOT an
// exact-equality guard: url.Parse + url.URL.String canonicalise the host's
// %XX escape (host has no Raw* twin), so %acookie is re-emitted as %ACookie
// regardless of redaction. The guarantee pinned here is the absence of a
// REDACTED marker (no over-redaction) plus the benign value surviving, not
// byte-for-byte fidelity.
func TestRedactAccessTokenURLKeepsOverlapHostFreeForm(t *testing.T) {
	cases := []struct {
		name           string
		raw            string
		wantBenignPart string // an access_token-free substring that must survive
	}{
		{"benign overlap host with port", "http://%acookie=ok:8443/path", "ok:8443/path"},
		{"benign overlap host no port", "http://%acookie=ok/path", "ok/path"},
		{"plain host untouched", "http://box:8443/path", "box:8443/path"},
		{"plain host no port untouched", "http://box/path", "box/path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if strings.Contains(got, accessTokenRedaction) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; gained a REDACTED marker "+
					"for an access_token-free host overlap",
					tc.raw, got)
			}
			if !strings.Contains(got, tc.wantBenignPart) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; benign substring %q lost",
					tc.raw, got, tc.wantBenignPart)
			}
		})
	}
}

// TestRedactAccessTokenErrorRedactsOverlapHost exercises the production
// reachability path the bug report identifies: --daemon-url /
// AF_DAEMON_URL feeds parseDaemonURL, whose "ws://" + u.Host form becomes the
// websocket dial URL; a dial failure produces a *url.Error whose .URL is
// routed through RedactAccessTokenError → RedactAccessTokenURL. An overlap
// access_token= in the host segment survives parseDaemonURL (it does no IDNA
// normalisation and accepts '=' in the host) and reached the redactor intact;
// before the fix only the query's access_token was redacted and the host
// secret leaked as %ACcess_token=<TOKEN> in the error string. Each case co-
// locates a query access_token so the asymmetry — same redactor call, query
// redacted but host not — is pinned away.
func TestRedactAccessTokenErrorRedactsOverlapHost(t *testing.T) {
	const hostTok = "af-sentinel-error-host-overlap"
	const queryTok = "af-sentinel-error-query-overlap"
	for _, tc := range []struct {
		name string
		raw  string
		want string // expected *url.Error.URL after redaction, embedded in err.Error()
	}{
		{
			name: "literal host + query token",
			raw:  "ws://access_token=" + hostTok + ":8443/v1/sessions/test/stream?access_token=" + queryTok,
			want: "ws://access_token=REDACTED:8443/v1/sessions/test/stream?access_token=REDACTED",
		},
		{
			name: "overlap host + query token",
			raw:  "ws://%access_token=" + hostTok + ":8443/v1/sessions/test/stream?access_token=" + queryTok,
			want: "ws://%ACcess_token=REDACTED:8443/v1/sessions/test/stream?access_token=REDACTED",
		},
		{
			name: "uppercase overlap host + query token",
			raw:  "ws://%ACcess_token=" + hostTok + ":8443/v1/sessions/test/stream?access_token=" + queryTok,
			want: "ws://%ACcess_token=REDACTED:8443/v1/sessions/test/stream?access_token=REDACTED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialErr := &url.Error{
				Op:  "Get",
				URL: tc.raw,
				Err: errors.New("dial tcp: no such host"),
			}
			got := RedactAccessTokenError(dialErr, hostTok)
			gotStr := got.Error()
			if strings.Contains(gotStr, hostTok) {
				t.Errorf("host token %q leaked into error: %s", hostTok, gotStr)
			}
			if strings.Contains(gotStr, queryTok) {
				t.Errorf("query token %q leaked into error: %s", queryTok, gotStr)
			}
			if !strings.Contains(gotStr, tc.want) {
				t.Errorf("error lost the redacted URL context: %s\nwant to contain %q",
					gotStr, tc.want)
			}
			// The host secret must not survive in any obfuscated form the
			// issuer serialises (e.g. %ACcess_token=<TOKEN>).
			if strings.Contains(strings.ToUpper(gotStr), strings.ToUpper(hostTok)) {
				t.Errorf("host token survived (case-insensitive): %s", gotStr)
			}
		})
	}
}

// TestRedactAccessTokenURLRedactsBracketedHostOverlap pins host-branch handling
// of a bracketed IP-literal authority (RFC 3986 §3.2.2 "[ ... ]") carrying an
// access_token= overlap. Inside the brackets ':' is an IPv6 field separator,
// not the host/port separator, so the value must NOT end at the first ':' —
// otherwise the IPv6-colon suffix of the bracketed literal survives the value
// scan and leaks ([%access_token=TOP:SECRET::1] → [%ACcess_token=REDACTED:
// SECRET::1]). The host branch selects ']' as the value terminator for a
// bracketed authority so the value runs to the close of the literal, redacting
// the remainder of the bracketed host exactly as the userinfo branch redacts
// through a structural end.
//
// Whether url.Parse rejects the bracketed access_token= authority outright is
// toolchain-patch-dependent: net/url's strict IP-literal host validation (the
// bracket is parsed as an address, which admits no '=') landed in go1.25.2, so
// there the parse fails and the public API fails closed to "[url redacted]";
// on the go.mod floor (go1.25.0/1.25.1) the parse succeeds and the host branch
// itself redacts the in-bracket value through its ']' terminator. The
// public-API row therefore pins the toolchain-independent property — no part
// of the token value survives, and the output is either the fail-closed marker
// or a URL whose in-bracket access_token value is REDACTED — rather than one
// exact string (#5103). The direct row pins that ']' — the terminator the host
// branch selects for a bracketed authority — redacts the whole in-bracket
// value, IPv6 colons included (a ':' terminator would truncate at the first one
// and leave the suffix).
func TestRedactAccessTokenURLRedactsBracketedHostOverlap(t *testing.T) {
	const secret = "af-sentinel-bracket-host-overlap"

	t.Run("public API keeps the secret out on every supported toolchain", func(t *testing.T) {
		raw := "http://[%access_token=" + secret + ":SECRET::1]/p"
		got := RedactAccessTokenURL(raw)
		if strings.Contains(got, secret) || strings.Contains(got, "SECRET") {
			t.Errorf("RedactAccessTokenURL(%q) = %q; token material survived", raw, got)
		}
		// The two accepted forms of a redacted result: go1.25.2+ rejects the
		// bracketed authority at url.Parse and fails closed; go1.25.0/1.25.1
		// (the go.mod floor) accept it and the host branch redacts the whole
		// in-bracket value through the ']' terminator. Either satisfies the
		// contract — pin the property, not a toolchain-specific string.
		switch got {
		case "[url redacted]":
		case "http://[%ACcess_token=REDACTED]/p":
		default:
			t.Errorf("RedactAccessTokenURL(%q) = %q; want %q (fail closed) or %q "+
				"(in-bracket access_token value redacted)",
				raw, got, "[url redacted]", "http://[%ACcess_token=REDACTED]/p")
		}
	})

	t.Run("bracket-aware terminator redacts full in-bracket value", func(t *testing.T) {
		// The terminator the host branch selects for a bracketed authority.
		// The value span runs to the closing ']', consuming the IPv6 ':' runs
		// a ':' terminator would split on.
		const bracketed = "[%access_token=" + secret + ":SECRET::1]"
		got, found := redactRawAccessTokenValue(bracketed, "]")
		if !found {
			t.Fatalf("redactRawAccessTokenValue(%q, %q) reported no match", bracketed, "]")
		}
		if strings.Contains(got, secret) {
			t.Errorf("redactRawAccessTokenValue(%q, \"]\") = %q; secret %q survived "+
				"(value truncated at an IPv6 colon instead of the closing ']')",
				bracketed, got, secret)
		}
		const want = "[%access_token=REDACTED]"
		if got != want {
			t.Errorf("redactRawAccessTokenValue(%q, \"]\")\n  got  %q\n  want %q",
				bracketed, got, want)
		}
	})
}
