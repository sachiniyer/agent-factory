package agentproto

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
