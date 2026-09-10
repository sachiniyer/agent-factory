package agentproto

import (
	"errors"
	"math/rand"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

func TestURLRedactedLeavesAccessTokenQueryUntouched(t *testing.T) {
	const token = "af-sentinel-url-redacted-trap"
	u, err := url.Parse("ws://box.test/stream?access_token=" + token)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if got := u.Redacted(); !strings.Contains(got, token) {
		t.Fatalf("url.URL.Redacted unexpectedly removed the query token: %q", got)
	}
}

func TestRedactAccessTokenURL(t *testing.T) {
	for _, tc := range []struct {
		name        string
		raw         string
		wantAbsent  []string
		wantPresent []string
	}{
		{
			name:        "token value replaced, everything else kept",
			raw:         "http://box:8080/v1/sessions/s/stream?tab=2&access_token=sekrit",
			wantAbsent:  []string{"sekrit"},
			wantPresent: []string{"box:8080", "/v1/sessions/s/stream", "tab=2", "REDACTED"},
		},
		{
			name:        "duplicate token values all replaced",
			raw:         "http://box:8080/stream?access_token=one&access_token=two",
			wantAbsent:  []string{"access_token=one", "access_token=two"},
			wantPresent: []string{"box:8080", "access_token=REDACTED"},
		},
		{
			name:        "semicolon separator preserves neighbouring parameter",
			raw:         "http://box:8080/stream?view=2;access_token=sekrit;mode=full",
			wantAbsent:  []string{"access_token=sekrit"},
			wantPresent: []string{"box:8080", "view=2", "access_token=REDACTED", "mode=full"},
		},
		{
			name:        "semicolon after empty token preserves neighbouring field",
			raw:         "http://box:8080/stream?access_token=;sekrit",
			wantAbsent:  []string{"access_token=;"},
			wantPresent: []string{"box:8080", "access_token=REDACTED", ";sekrit"},
		},
		{
			name:        "fragment token redacted",
			raw:         "http://box:8080/callback#access_token=sekrit",
			wantAbsent:  []string{"sekrit"},
			wantPresent: []string{"box:8080", "/callback", "access_token=REDACTED"},
		},
		{
			name:        "fragment token redacted alongside a query token",
			raw:         "http://box:8080/callback?tab=2&access_token=query-sekrit#access_token=fragment-sekrit",
			wantAbsent:  []string{"query-sekrit", "fragment-sekrit"},
			wantPresent: []string{"box:8080", "tab=2", "access_token=REDACTED"},
		},
		{
			name:        "no token untouched",
			raw:         "http://box:8080/v1/sessions/s/stream?tab=2",
			wantAbsent:  []string{"REDACTED"},
			wantPresent: []string{"box:8080", "tab=2"},
		},
		{
			name:        "unparseable url says nothing",
			raw:         "http://box:8080/\x7f?access_token=sekrit",
			wantAbsent:  []string{"sekrit"},
			wantPresent: []string{"redacted"},
		},
		{
			// #4161: a nested/callback URL arrives percent-encoded, so RawQuery
			// carries no literal access_token= for the text fallback and the
			// component sweep never reads RawQuery. The identical payload was
			// already redacted in a fragment and in a path; only the query leaked.
			name:        "token inside a percent-encoded query value",
			raw:         "https://af.host/open?next=%2Fws%3Faccess_token%3Dsekrit",
			wantAbsent:  []string{"sekrit"},
			wantPresent: []string{"af.host", "/open", "next=", "REDACTED"},
		},
		{
			// The neighbouring parameter must survive: redacting a value must not
			// coarsen into dropping or rewriting the rest of the query.
			name:        "percent-encoded query value redacted, neighbour kept",
			raw:         "https://af.host/o?keep=hello&next=%2Fws%3Faccess_token%3Dsekrit",
			wantAbsent:  []string{"sekrit"},
			wantPresent: []string{"keep=hello", "REDACTED"},
		},
		{
			// The unencoded spelling already worked, via the literal-needle text
			// fallback rather than the value scan. Pin it so a future refactor of
			// either path cannot silently drop the one it does not touch.
			name:        "token inside a plain nested query value",
			raw:         "https://af.host/open?next=/ws?access_token=sekrit",
			wantAbsent:  []string{"sekrit"},
			wantPresent: []string{"af.host", "REDACTED"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("RedactAccessTokenURL(%q) = %q, must not contain %q", tc.raw, got, absent)
				}
			}
			for _, want := range tc.wantPresent {
				if !strings.Contains(got, want) {
					t.Errorf("RedactAccessTokenURL(%q) = %q, want it to contain %q", tc.raw, got, want)
				}
			}
		})
	}
}

func TestRedactAccessTokenURLRedactsEveryKeyCase(t *testing.T) {
	got := RedactAccessTokenURL(
		"https://box.test/stream?access_token=lower-secret&Access_Token=upper-secret")
	for _, token := range []string{"lower-secret", "upper-secret"} {
		if strings.Contains(got, token) {
			t.Fatalf("RedactAccessTokenURL retained token value %q: %s", token, got)
		}
	}
}

// TestRedactAccessTokenURLRedactsEveryComponent is the structural counterpart to
// the byte sweep below. Structured query redaction reaches the query and nothing
// else, so every OTHER component of a url.URL is a place a token can ride past
// it — the fragment of an implicit-grant callback most realistically (#2771).
// The set of components is closed, so sweeping it is a claim the next added
// field cannot quietly falsify, unlike "the separators we thought of".
func TestRedactAccessTokenURLRedactsEveryComponent(t *testing.T) {
	// Every case here carries a query token, exercising the query-match branch
	// where the component sweep always ran (the path that always worked).
	for _, tc := range []struct {
		component string
		raw       string
	}{
		{"fragment", "http://box:8080/callback?access_token=q-sekrit#access_token=component-sekrit"},
		{"path", "http://box:8080/access_token=component-sekrit?access_token=q-sekrit"},
		{"host", "http://access_token=component-sekrit/stream?access_token=q-sekrit"},
		{"userinfo", "http://user:access_token=component-sekrit@box:8080/?access_token=q-sekrit"},
		{"opaque", "mailto:access_token=component-sekrit?access_token=q-sekrit"},
	} {
		t.Run(tc.component, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			for _, secret := range []string{"component-sekrit", "q-sekrit"} {
				if strings.Contains(got, secret) {
					t.Errorf("RedactAccessTokenURL(%q) = %q, %s token %q survived",
						tc.raw, got, tc.component, secret)
				}
			}
		})
	}
}

// TestRedactAccessTokenURLRedactsPercentEncodedComponentKey is the regression
// guard for the leak that motivated un-gating the component sweep. When a
// URL's only access_token lives in a component (fragment / path / userinfo) and
// its key is percent-encoded (%61ccess_token=), the literal-needle text pass
// cannot see the decoded key — only the component sweep, which reads url.URL's
// percent-DECODED fields, can. Nested encoding is included because one parser
// decode can leave another encoded key in the component. The sweep used to be
// gated behind redactAccessTokenQuery, so a component-only percent-encoded key
// took the early return and survived the redaction boundary verbatim. These
// cases deliberately carry NO query access_token so they exercise the no-query
// branch where the sweep was previously skipped; the last case carries one to
// guard the query-match branch still redacts the encoded component.
func TestRedactAccessTokenURLRedactsPercentEncodedComponentKey(t *testing.T) {
	cases := []struct {
		component string
		raw       string
	}{
		{"fragment", "http://box:8080/callback#%61ccess_token=component-sekrit"},
		{"fragment-double-encoded", "http://box:8080/callback#%2561ccess%255Ftoken=component-sekrit"},
		{"path", "http://box:8080/%61ccess_token=component-sekrit"},
		{"path-double-encoded", "http://box:8080/%2561ccess%255Ftoken=component-sekrit"},
		{"userinfo", "http://user:%61ccess_token=component-sekrit@box:8080/"},
		{"userinfo-double-encoded", "http://user:%2561ccess%255Ftoken=component-sekrit@box:8080/"},
		{"fragment-with-query-token", "http://box:8080/callback?access_token=q#%61ccess_token=component-sekrit"},
		// Opaque URLs: url.Parse does NOT decode u.Opaque, so the literal-needle
		// scan misses %61ccess_token=.  The fix decodes before scanning.
		{"opaque", "mailto:%61ccess_token=component-sekrit"},
		{"opaque-double-encoded", "mailto:%2561ccess%255Ftoken=component-sekrit"},
		{"opaque-literal", "mailto:access_token=component-sekrit"},
		// Malformed escape in opaque: url.PathUnescape returns an error, so the
		// fail-closed branch must redact the whole opaque rather than emitting it.
		{"opaque-malformed-escape", "mailto:%zz-component-sekrit"},
	}
	for _, tc := range cases {
		t.Run(tc.component, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if strings.Contains(got, "component-sekrit") || strings.Contains(got, "q-sekrit") {
				t.Errorf("RedactAccessTokenURL(%q) = %q; %s token survived",
					tc.raw, got, tc.component)
			}
			if !strings.Contains(got, accessTokenRedaction) {
				t.Errorf("RedactAccessTokenURL(%q) = %q; want redaction marker",
					tc.raw, got)
			}
		})
	}
}

// TestRedactAccessTokenTextRedactsAfterEveryPrecedingByte sweeps the exact
// dimension this redactor keeps losing credentials on. The field match used to
// be gated on a hand-listed set of separators that may precede it, so the
// default answer for an unlisted byte was "leave the credential alone": ';' had
// to be added in #2687 and '#' in #2771. A sweep of the whole byte space is the
// only version of this test that cannot go stale the same way.
func TestRedactAccessTokenTextRedactsAfterEveryPrecedingByte(t *testing.T) {
	// No value terminator in the token, so a byte that fails to start the
	// redaction leaves the whole sentinel visible.
	const token = "af-sentinel-preceding-byte-sweep"
	for b := 0; b < 256; b++ {
		preceding := byte(b)
		// []byte, not string(preceding): the latter would UTF-8 encode anything
		// above 0x7f and never place the raw byte in front of the field.
		input := "context" + string([]byte{preceding}) + AccessTokenQueryParam + "=" + token
		got := RedactAccessTokenText(input)
		if strings.Contains(got, token) {
			t.Errorf("RedactAccessTokenText(%q) = %q; token survived after preceding byte %#02x",
				input, got, preceding)
		}
		if !strings.Contains(got, accessTokenRedaction) {
			t.Errorf("RedactAccessTokenText(%q) = %q; want the redaction marker after preceding byte %#02x",
				input, got, preceding)
		}
	}
}

func TestRedactAccessTokenError(t *testing.T) {
	const token = "af-sentinel-error-redaction"
	cause := errors.New("connection refused")
	dialErr := &url.Error{
		Op:  "Get",
		URL: "ws://box:8080/stream?tab=2&access_token=" + token,
		Err: cause,
	}

	got := RedactAccessTokenError(dialErr, token)
	if strings.Contains(got.Error(), token) {
		t.Fatalf("structured dial error retained token: %s", got)
	}
	if !strings.Contains(got.Error(), "box:8080") || !strings.Contains(got.Error(), "REDACTED") {
		t.Fatalf("structured dial error lost useful URL context: %s", got)
	}
	if !errors.Is(got, cause) {
		t.Fatalf("structured redaction lost the original error chain: %v", got)
	}

	plain := errors.New("transport failure carrying token " + token)
	got = RedactAccessTokenError(plain, token)
	if strings.Contains(got.Error(), token) || !strings.Contains(got.Error(), "REDACTED") {
		t.Fatalf("unstructured error was not safely redacted: %s", got)
	}
	clean := errors.New("connection refused")
	if got := RedactAccessTokenError(clean, token); got != clean {
		t.Fatalf("clean error identity changed: %v", got)
	}
	if RedactAccessTokenError(nil, token) != nil {
		t.Fatal("nil error must remain nil")
	}

	shortURL := &url.Error{
		Op:  "Get",
		URL: "token",
		Err: errors.New("failure access_token=sekrit"),
	}
	if got := RedactAccessTokenError(shortURL, ""); strings.Contains(got.Error(), "sekrit") {
		t.Fatalf("short structured URL split the redaction needle: %s", got)
	}
}

func TestRedactAccessTokenText(t *testing.T) {
	const token = "af-sentinel-log-redaction"
	in := "access_token=first Get \"ws://box/stream?tab=2&access_token=" + token + "\": dial failed"
	got := RedactAccessTokenText(in)
	if strings.Contains(got, token) || strings.Contains(got, "first") {
		t.Fatalf("text redactor retained token: %s", got)
	}
	for _, want := range []string{"box", "tab=2", "access_token=REDACTED", "dial failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("text redactor lost %q: %s", want, got)
		}
	}
}

type accessTokenRedactionCase struct {
	Input string
	Token string
}

func TestRedactAccessTokenTextNeverRetainsTokenValue(t *testing.T) {
	const generatedCases = 3 * 4 * 6 * 3 * 10
	caseNumber := 0
	config := &quick.Config{
		MaxCount: generatedCases,
		Rand:     rand.New(rand.NewSource(2695)),
		Values: func(values []reflect.Value, source *rand.Rand) {
			generated := generateAccessTokenRedactionCase(caseNumber, source)
			caseNumber++
			values[0] = reflect.ValueOf(generated)
		},
	}

	property := func(generated accessTokenRedactionCase) bool {
		got := RedactAccessTokenText(generated.Input)
		if strings.Contains(got, generated.Token) {
			t.Logf("RedactAccessTokenText(%q) = %q; token value %q survived",
				generated.Input, got, generated.Token)
			return false
		}
		return true
	}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}

func generateAccessTokenRedactionCase(n int, source *rand.Rand) accessTokenRedactionCase {
	caseID := n
	position := n % 10
	n /= 10
	// '#' is deliberately absent: it is a value TERMINATOR, so a token shaped
	// around one is genuinely two fields, and the fragment half gets its own
	// match. Its role as a field PREFIX is swept exhaustively by
	// TestRedactAccessTokenTextRedactsAfterEveryPrecedingByte instead.
	separators := []string{";", "&", "?"}
	shapes := []func(string, string, string) string{
		func(separator, marker, decoration string) string {
			return separator + marker + decoration
		},
		func(separator, marker, decoration string) string {
			return marker + separator + decoration
		},
		func(separator, marker, decoration string) string {
			return marker + decoration + separator
		},
		func(separator, marker, decoration string) string {
			return accessTokenRedaction + separator + marker + decoration
		},
	}
	decorations := []string{"", "=", "%3F", "%26", "%3B", "%3D"}
	parameterNames := []string{"access_token", "Access_Token", "ACCESS_TOKEN"}

	separator := separators[n%len(separators)]
	n /= len(separators)
	shape := shapes[n%len(shapes)]
	n /= len(shapes)
	decoration := decorations[n%len(decorations)]
	n /= len(decorations)
	parameterName := parameterNames[n%len(parameterNames)]

	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	randomPart := make([]byte, 12)
	for i := range randomPart {
		randomPart[i] = letters[source.Intn(len(letters))]
	}
	marker := "af-token-" + strconv.Itoa(caseID) + "-" + string(randomPart)
	token := shape(separator, marker, decoration)
	field := parameterName + "=" + token
	inputs := []string{
		"https://box.test/stream?before=1;" + field + ";after=2",
		"https://box.test/stream?" + field,
		"https://box.test/stream?" + field + "&after=trailing",
		"https://box.test/stream?before=1&" + field + "&after=2",
		"https://box.test/stream?before=1&" + field,
		"Get \"ws://box.test/stream?" + field + "\": dial failed",
		"failure " + field + " # fragment",
		"https://box.test/stream?" + field + "&before=1&" + field,
		// Implicit-grant callbacks put the credential in the fragment, where no
		// query separator precedes it at all (#2771).
		"https://box.test/callback#" + field,
		"https://box.test/callback?tab=2#" + field,
	}
	return accessTokenRedactionCase{Input: inputs[position], Token: token}
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"standard", "Bearer abc123", "abc123"},
		{"case_insensitive_scheme", "bearer abc123", "abc123"},
		{"trims_space", "Bearer   abc123  ", "abc123"},
		{"empty", "", ""},
		{"no_scheme", "abc123", ""},
		{"wrong_scheme", "Basic abc123", ""},
		{"scheme_only", "Bearer ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BearerToken(tc.in); got != tc.want {
				t.Errorf("BearerToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAccessTokenFromQuery(t *testing.T) {
	q := url.Values{}
	q.Set(AccessTokenQueryParam, "tok-xyz")
	if got := AccessTokenFromQuery(q); got != "tok-xyz" {
		t.Errorf("AccessTokenFromQuery = %q, want %q", got, "tok-xyz")
	}
	if got := AccessTokenFromQuery(url.Values{}); got != "" {
		t.Errorf("AccessTokenFromQuery(empty) = %q, want empty", got)
	}
}

// TestRedactAccessTokenURLLeavesUnmatchedOpaqueEncoded is the #4161 secondary
// half: url.URL.String prints Opaque verbatim and there is no RawOpaque to
// re-escape from, so writing the DECODED opaque back unconditionally rewrote
// every opaque URL that carried no credential at all.
func TestRedactAccessTokenURLLeavesUnmatchedOpaqueEncoded(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"encoded at-sign survives", "mailto:user%40host.example"},
		{"encoded slash and space survive", "af:a%2Fb%20c"},
		{"unencoded opaque untouched", "mailto:plain@host.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactAccessTokenURL(tc.raw); got != tc.raw {
				t.Errorf("RedactAccessTokenURL(%q) = %q, want it returned unchanged: "+
					"no credential matched, so the opaque must keep its original escaping", tc.raw, got)
			}
		})
	}
}

// TestRedactAccessTokenURLStillRedactsOpaque pins that the fidelity fix above
// did not cost the redaction it guards.
func TestRedactAccessTokenURLStillRedactsOpaque(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"percent-encoded key", "mailto:%61ccess_token=component-sekrit"},
		{"literal key", "mailto:access_token=component-sekrit"},
		{"malformed escape redacts whole field", "mailto:%zz-component-sekrit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAccessTokenURL(tc.raw)
			if strings.Contains(got, "sekrit") {
				t.Errorf("RedactAccessTokenURL(%q) = %q, must not contain the secret", tc.raw, got)
			}
		})
	}
}
