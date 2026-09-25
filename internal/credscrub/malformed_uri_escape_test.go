package credscrub

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/redactx"
)

// TestNonCredKeyMalformedCarrierNotRecoverable gates the end-to-end impact
// of the uri.go malformed-drop bug across all three leaking branches: a
// credential-shape value under a non-credential query key (ref=), in a
// keyless fragment (#), or in the path, percent-encoded with a trailing
// malformed %, must not ship a carrier from which PercentDecode recovers
// the plaintext.
func TestNonCredKeyMalformedCarrierNotRecoverable(t *testing.T) {
	const pat = "ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	single := pctScrub(pat)
	carriers := []struct {
		name  string
		in    string
		after string
		plus  bool
	}{
		{"query-single", "fetch http://h/?ref=" + single + "%", "?ref=", true},
		{"frag-single", "http://h/p#" + single + "%", "#", false},
		{"path-single", "http://h/" + single + "%", "http://h/", false},
	}
	for _, c := range carriers {
		t.Run(c.name, func(t *testing.T) {
			out := Scrub(c.in)
			// If the carrier was redacted away (the marker replaced the
			// encoded value), the after-substring is gone — the leak is
			// closed.
			i := strings.Index(out, c.after)
			if i < 0 {
				return
			}
			leaked := out[i+len(c.after):]
			v, _ := redactx.PercentDecode(leaked, c.plus)
			if strings.Contains(v.Text, pat) {
				t.Fatalf("%s: shipped carrier decodes to plaintext PAT: %q", c.name, v.Text)
			}
		})
	}
}

// TestNonCredKeyMalformedCarrierControlGate pairs the fix with its control:
// the well-formed (no trailing %) counterpart must still be redacted, so the
// fix did not widen the valid-decode path.
func TestNonCredKeyMalformedCarrierControlGate(t *testing.T) {
	const pat = "ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	single := pctScrub(pat)
	well := "fetch http://h/?ref=" + single
	out := Scrub(well)
	if strings.Contains(out, pat) {
		t.Fatalf("well-formed query shipped the PAT: %q", out)
	}
	if !strings.Contains(out, SecretMarker) {
		t.Fatalf("well-formed query was not redacted: %q", out)
	}
}

func pctScrub(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteByte('%')
		b.WriteByte(hex[s[i]>>4])
		b.WriteByte(hex[s[i]&0xF])
	}
	return b.String()
}
