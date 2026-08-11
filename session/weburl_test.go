package session

import "testing"

func TestNormalizeWebTabURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "bare host:port defaults to http", in: "localhost:3000", want: "http://localhost:3000"},
		{name: "loopback ip host:port", in: "127.0.0.1:5173", want: "http://127.0.0.1:5173"},
		{name: "full http url kept", in: "http://localhost:8080/app", want: "http://localhost:8080/app"},
		{name: "external https url kept", in: "https://example.com/x", want: "https://example.com/x"},
		{name: "rooted fqdn keeps one trailing label", in: "https://example.com./x", want: "https://example.com./x"},
		{name: "ideographic dot in host", in: "http://127。0。0。1:3000", want: "http://127.0.0.1:3000"},
		{name: "fullwidth dot in host", in: "http://127．0．0．1:3000", want: "http://127.0.0.1:3000"},
		{name: "halfwidth dot in host", in: "http://127｡0｡0｡1:3000", want: "http://127.0.0.1:3000"},
		{name: "domain separators stay scoped to host", in: "https://example。com/path。part", want: "https://example.com/path%E3%80%82part"},
		{name: "fullwidth localhost", in: "http://ｌｏｃａｌｈｏｓｔ:3000", want: "http://localhost:3000"},
		{name: "unicode domain becomes ascii", in: "https://bücher.example/path", want: "https://xn--bcher-kva.example/path"},
		{name: "short legacy ipv4 becomes dotted", in: "http://127.1:3000", want: "http://127.0.0.1:3000"},
		{name: "integer legacy ipv4 becomes dotted", in: "http://134744072", want: "http://8.8.8.8"},
		{name: "numeric label before dns suffix stays a domain", in: "https://09.example/path", want: "https://09.example/path"},
		{name: "ipv6 becomes browser canonical", in: "http://[2001:0db8::0001]:8080", want: "http://[2001:db8::1]:8080"},
		{name: "mapped ipv6 remains an ipv6 origin", in: "http://[::ffff:127.0.0.1]:3000", want: "http://[::ffff:7f00:1]:3000"},
		{name: "leading-zero default port is removed", in: "http://example.com:00080/path", want: "http://example.com/path"},
		{name: "maximum port is valid", in: "https://example.com:65535", want: "https://example.com:65535"},
		{name: "whitespace trimmed", in: "  localhost:3000 ", want: "http://localhost:3000"},
		{name: "empty rejected", in: "   ", wantErr: true},
		{name: "non-http scheme rejected", in: "ftp://host/x", wantErr: true},
		{name: "file scheme rejected", in: "file:///etc/passwd", wantErr: true},
		{name: "invalid octal numeric host rejected", in: "http://09:3000", wantErr: true},
		{name: "overflowing numeric component rejected", in: "http://1.2.3.999:3000", wantErr: true},
		{name: "overflowing port rejected", in: "https://example.com:65536", wantErr: true},
		{name: "zoned ipv6 rejected", in: "http://[fe80::1%25eth0]:3000", wantErr: true},
		{name: "ipvfuture host rejected", in: "http://[v1.foo]:3000", wantErr: true},
		{name: "bracketed ipv4 rejected", in: "http://[127.0.0.1]:3000", wantErr: true},
		{name: "idna-ignored-only host rejected", in: "https://\u00ad/x", wantErr: true},
		{name: "root-only domain rejected", in: "https://./x", wantErr: true},
		{name: "leading empty domain label rejected", in: "https://.example.com/x", wantErr: true},
		{name: "interior empty domain label rejected", in: "https://example..com/x", wantErr: true},
		{name: "multiple trailing empty labels rejected", in: "https://example.com../x", wantErr: true},
		{name: "idna-mapped host delimiter rejected", in: "https://example／evil.com/x", wantErr: true},
		{name: "idna-mapped port delimiter rejected", in: "https://example.com：443/x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeWebTabURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeWebTabURL(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeWebTabURL(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeWebTabURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateBrowserDomainHostRejectsURLStandardForbiddenCodePoints(t *testing.T) {
	for _, forbidden := range []rune{
		0x00, 0x01, 0x1f, ' ', '#', '%', '/', ':', '<', '>', '?', '@', '[', '\\', ']', '^', '|', 0x7f,
	} {
		host := "example" + string(forbidden) + ".com"
		if err := validateBrowserDomainHost(host); err == nil {
			t.Errorf("validateBrowserDomainHost(%q) accepted forbidden code point U+%04X", host, forbidden)
		}
	}

	for _, valid := range []string{"example.com", "example.com.", "_service.example"} {
		if err := validateBrowserDomainHost(valid); err != nil {
			t.Errorf("validateBrowserDomainHost(%q) unexpected error: %v", valid, err)
		}
	}
}

func TestWebTabURLForPort(t *testing.T) {
	got, err := WebTabURLForPort(3000)
	if err != nil {
		t.Fatalf("WebTabURLForPort(3000): %v", err)
	}
	if got != "http://localhost:3000" {
		t.Fatalf("WebTabURLForPort(3000) = %q, want http://localhost:3000", got)
	}
	for _, bad := range []int{0, -1, 70000} {
		if _, err := WebTabURLForPort(bad); err == nil {
			t.Fatalf("WebTabURLForPort(%d) = nil error, want error", bad)
		}
	}
}

func TestIsLoopbackWebTarget(t *testing.T) {
	loopback := []string{
		"http://localhost:3000",
		"http://127.0.0.1:5173",
		"http://127.0.0.53/x",
		"http://[::1]:8080",
		"http://[::ffff:7f00:1]:3000",
		// Rooted (trailing-dot) FQDN forms of the same loopback hosts — a
		// browser treats "localhost." exactly as "localhost" (#2004).
		"http://localhost.:3000",
		"http://127.0.0.1.:5173",
	}
	for _, u := range loopback {
		if !IsLoopbackWebTarget(u) {
			t.Errorf("IsLoopbackWebTarget(%q) = false, want true", u)
		}
	}
	external := []string{
		"https://example.com",
		"http://192.168.1.10:3000",
		"http://10.0.0.1/x",
		"not a url",
		// A rooted external name is still external — stripping the root dot
		// must not turn a real remote host into loopback.
		"https://example.com.",
	}
	for _, u := range external {
		if IsLoopbackWebTarget(u) {
			t.Errorf("IsLoopbackWebTarget(%q) = true, want false", u)
		}
	}
}

// TestIsLoopbackHostTrailingDot pins the loopback classifier directly on the
// rooted-FQDN forms (#2004): "localhost." and "127.0.0.1." are the same host as
// their unrooted forms and are loopback, while a doubled dot or a bare dot is
// malformed and must fail closed as non-loopback.
func TestIsLoopbackHostTrailingDot(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"localhost.", true},
		{"LocalHost.", true},
		{"127.0.0.1", true},
		{"127.0.0.1.", true},
		{"::1", true},
		{"example.com", false},
		{"example.com.", false},
		{"", false},
		{".", false},
		{"localhost..", false},
	}
	for _, c := range cases {
		if got := isLoopbackHost(c.host); got != c.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
