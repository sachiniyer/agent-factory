package agentproto

import (
	"strings"
	"testing"
)

func TestRedactAccessTokenURLRawQueryCrossProduct(t *testing.T) {
	keys := []struct {
		name string
		raw  string
	}{
		{name: "literal", raw: "access_token"},
		{name: "percent encoded", raw: "%61ccess%5Ftoken"},
		{name: "mixed case", raw: "AcCeSs_ToKeN"},
		{name: "doubly encoded", raw: "%2561ccess%255Ftoken"},
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

func TestRedactAccessTokenURLRawQueryDoesNotDependOnPairParsing(t *testing.T) {
	const input = "http://localhost:3000/?before=1&%61ccess_token=TOKSECRET%zz&after=2"
	const want = "http://localhost:3000/?before=1&%61ccess_token=REDACTED&after=2"
	if got := RedactAccessTokenURL(input); got != want {
		t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
	}
}

func TestRedactAccessTokenURLMapsNestedEncodingBackToOriginalQuery(t *testing.T) {
	const input = "http://localhost:3000/?keep=a%2Fb&next=%252Fws%253F%2561ccess%255Ftoken%253DTOKSECRET&after=z%2Bz"
	const want = "http://localhost:3000/?keep=a%2Fb&next=%252Fws%253F%2561ccess%255Ftoken%253DREDACTED&after=z%2Bz"
	if got := RedactAccessTokenURL(input); got != want {
		t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
	}
}

func TestRedactAccessTokenURLKeepsDecodedPlusInsideNestedToken(t *testing.T) {
	const input = "http://localhost:3000/?next=%61ccess_token%3DTOK%252BSECRET&after=2"
	const want = "http://localhost:3000/?next=%61ccess_token%3DREDACTED&after=2"
	if got := RedactAccessTokenURL(input); got != want {
		t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
	}
}

func TestRedactAccessTokenURLMapsNestedEmptyValueBoundary(t *testing.T) {
	const input = "http://localhost:3000/?next=%61ccess_token%3D&after=2"
	const want = "http://localhost:3000/?next=%61ccess_token%3DREDACTED&after=2"
	if got := RedactAccessTokenURL(input); got != want {
		t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
	}
}

func TestRedactAccessTokenURLKeepsDecodedTextTerminatorsInsideURIValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		encoded string
	}{
		{name: "space", encoded: "%2520"},
		{name: "newline", encoded: "%250A"},
		{name: "double quote", encoded: "%2522"},
		{name: "single quote", encoded: "%2527"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "http://localhost:3000/?next=%2Fws%3Faccess_token%3Dprefix" +
				tc.encoded + "sensitive-suffix&after=2"
			const want = "http://localhost:3000/?next=%2Fws%3Faccess_token%3DREDACTED&after=2"
			if got := RedactAccessTokenURL(input); got != want {
				t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestFullyPercentDecodedViewHandlesDeepNesting(t *testing.T) {
	const depth = 4096
	raw := "%" + strings.Repeat("25", depth-1) + "61ccess_token"
	view, malformed := fullyPercentDecodedView(raw, false)
	if malformed {
		t.Fatal("fullyPercentDecodedView(deep key) reported a malformed raw escape")
	}
	if got, want := percentDecodedText(view), AccessTokenQueryParam; got != want {
		t.Fatalf("fullyPercentDecodedView(deep key) = %q, want %q", got, want)
	}
	if got := view[0]; got.sourceStart != 0 || got.sourceEnd != 1+2*depth {
		t.Fatalf("deeply decoded byte source = [%d,%d), want [0,%d)",
			got.sourceStart, got.sourceEnd, 1+2*depth)
	}
}

// Percent bytes have no encoding semantics in arbitrary prose. URL callers
// must use RedactAccessTokenURL, whose parser establishes that provenance.
func TestRedactAccessTokenTextDoesNotGuessPercentEncoding(t *testing.T) {
	const input = "diagnostic %61ccess_token=TOKSECRET"
	if got := RedactAccessTokenText(input); got != input {
		t.Fatalf("RedactAccessTokenText(%q) = %q, want input unchanged", input, got)
	}
}
