package agentproto

import (
	"net/http"
	"net/url"
	"testing"
)

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

func TestTokenFromRequest(t *testing.T) {
	// Header path.
	r := &http.Request{Header: http.Header{}, URL: &url.URL{}}
	r.Header.Set(AuthHeader, "Bearer header-tok")
	if got := TokenFromRequest(r); got != "header-tok" {
		t.Errorf("header path = %q, want header-tok", got)
	}

	// Query fallback (the browser WS path — no header).
	r2 := &http.Request{Header: http.Header{}, URL: &url.URL{RawQuery: AccessTokenQueryParam + "=query-tok"}}
	if got := TokenFromRequest(r2); got != "query-tok" {
		t.Errorf("query fallback = %q, want query-tok", got)
	}

	// Header wins over query when both are present.
	r3 := &http.Request{Header: http.Header{}, URL: &url.URL{RawQuery: AccessTokenQueryParam + "=query-tok"}}
	r3.Header.Set(AuthHeader, "Bearer header-tok")
	if got := TokenFromRequest(r3); got != "header-tok" {
		t.Errorf("header precedence = %q, want header-tok", got)
	}

	// Neither present.
	r4 := &http.Request{Header: http.Header{}, URL: &url.URL{}}
	if got := TokenFromRequest(r4); got != "" {
		t.Errorf("no auth = %q, want empty", got)
	}
}
