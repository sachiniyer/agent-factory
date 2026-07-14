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
		{name: "whitespace trimmed", in: "  localhost:3000 ", want: "http://localhost:3000"},
		{name: "empty rejected", in: "   ", wantErr: true},
		{name: "non-http scheme rejected", in: "ftp://host/x", wantErr: true},
		{name: "file scheme rejected", in: "file:///etc/passwd", wantErr: true},
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
	}
	for _, u := range external {
		if IsLoopbackWebTarget(u) {
			t.Errorf("IsLoopbackWebTarget(%q) = true, want false", u)
		}
	}
}
