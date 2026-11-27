package session

import (
	"strings"
	"testing"
)

// #7889: af's own provisioning execs inherit the container env where
// `-e SSL_CERT_FILE=` stands because `docker run` cannot unset an image ENV.
// The clone step's git is the one provisioning child that can open a TLS
// connection, so the exec helpers prefix each script with an unset of the
// pinned path-valued trust names — the env-honoring TLS stacks then fall back
// to the compiled platform store instead of loading the pin's empty path.
func TestTrustPathUnpinnedScriptUnsetsPinnedTrustNames(t *testing.T) {
	p := &dockerProvisioner{credentialEnvPins: []string{
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
		"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "CODEX_CA_CERTIFICATE",
		"HTTP_PROXY",
	}}
	got := p.trustPathUnpinnedScript("git clone https://example.invalid/r.git")
	for _, name := range []string{
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
		"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "CODEX_CA_CERTIFICATE",
	} {
		if !strings.Contains(got, " "+name) {
			t.Fatalf("script must unset pinned trust name %s: %q", name, got)
		}
	}
	if !strings.HasPrefix(got, "unset ") {
		t.Fatalf("the unset must lead the script so every later command sees it: %q", got)
	}
	if !strings.HasSuffix(got, "git clone https://example.invalid/r.git") {
		t.Fatalf("the operator's script must run verbatim after the preamble: %q", got)
	}
	// A non-path pin is not a TLS-store reader — empty is its neutral form,
	// so unsetting it would only change proxy semantics for no benefit.
	for _, name := range strings.Fields(strings.TrimPrefix(strings.SplitN(got, ";", 2)[0], "unset ")) {
		if name == "HTTP_PROXY" {
			t.Fatalf("a non-path pin must keep its defined-empty claim, not be unset: %q", got)
		}
	}
}

// An operator-forwarded trust name is NOT a pin — credentialEnvPins excludes
// it — and the clone needs that forwarded CA to reach a private-CA remote.
// The preamble must leave it alone.
func TestTrustPathUnpinnedScriptKeepsForwardedValues(t *testing.T) {
	p := &dockerProvisioner{credentialEnvPins: []string{"SSL_CERT_DIR", "HTTP_PROXY"}}
	got := p.trustPathUnpinnedScript("true")
	if strings.Contains(got, "SSL_CERT_FILE") {
		t.Fatalf("an unpinned (operator-forwarded) trust name must not be unset: %q", got)
	}
	if !strings.Contains(got, "SSL_CERT_DIR") {
		t.Fatalf("a pinned trust name must be unset: %q", got)
	}
}

// Without credential mounts there are no pins at all — the script must be
// byte-identical so the preamble cannot perturb a session that carries none.
func TestTrustPathUnpinnedScriptNoPinsIsIdentity(t *testing.T) {
	p := &dockerProvisioner{}
	const script = "chmod +x /opt/af/bin/af"
	if got := p.trustPathUnpinnedScript(script); got != script {
		t.Fatalf("a session with no pins must pass its script through verbatim: %q", got)
	}
}
