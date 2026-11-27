package session

import (
	"slices"
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

// The Requests/curl CA-bundle chain is a precedence pair: Requests merges
// `REQUESTS_CA_BUNDLE or CURL_CA_BUNDLE or verify`, so a higher-precedence
// REQUESTS_CA_BUNDLE af sets — even to a real platform bundle — would
// outrank a CURL_CA_BUNDLE the operator forwarded for a private CA (#7889
// review). Forwarding either member must exempt BOTH from the pin, exactly
// like the proxy case twins.
func TestCredentialMountPinnedNames_TrustBundleSiblingExemptsBoth(t *testing.T) {
	for _, pair := range credentialMountTrustBundlePairs {
		for _, spelling := range pair {
			sibling := pair[0]
			if sibling == spelling {
				sibling = pair[1]
			}
			pinned := credentialMountPinnedNames("opencode", []string{spelling})
			if slices.Contains(pinned, spelling) {
				t.Fatalf("forwarded %s must not be pinned", spelling)
			}
			if slices.Contains(pinned, sibling) {
				t.Fatalf("forwarding %s must also exempt its precedence sibling %s", spelling, sibling)
			}
		}
	}
	// Unforwarded, both stay pinned — the resolved-path pin only matters
	// when the operator did not claim the chain.
	pinned := credentialMountPinnedNames("opencode", []string{"OPERATOR_THING"})
	for _, pair := range credentialMountTrustBundlePairs {
		for _, spelling := range pair {
			if !slices.Contains(pinned, spelling) {
				t.Fatalf("unforwarded trust-bundle name %s must stay pinned", spelling)
			}
		}
	}
}

// The unforwarded sibling cannot simply drop the pin — its name is in the
// inner filter's always-admitted set, so an image value for it would reach
// the agent. It is reseated to the operator's forwarded bundle through the
// twin machinery instead (bare `-e NAME` + synthesized NAME=V in the docker
// client env, so the value never enters argv).
func TestCredentialMountTrustBundleForwarding_ReseatsToForwardedBundle(t *testing.T) {
	environ := []string{
		"CURL_CA_BUNDLE=/operator/private-ca.pem",
		"UNRELATED=1",
	}
	names, synth := credentialMountTwinForwarding(credentialMountTrustBundlePairs, []string{"CURL_CA_BUNDLE"}, environ)
	if len(names) != 1 || names[0] != "REQUESTS_CA_BUNDLE" {
		t.Fatalf("names = %v, want REQUESTS_CA_BUNDLE emitted for the reseat", names)
	}
	if len(synth) != 1 || synth[0] != "REQUESTS_CA_BUNDLE=/operator/private-ca.pem" {
		t.Fatalf("synth = %v, want REQUESTS_CA_BUNDLE reseated to the forwarded bundle", synth)
	}

	// The reverse direction claims the chain for a forwarded REQUESTS too:
	// curl (which reads only CURL_CA_BUNDLE) gets the operator's CA rather
	// than its compiled default, the same claim the proxy twins make.
	names, synth = credentialMountTwinForwarding(credentialMountTrustBundlePairs,
		[]string{"REQUESTS_CA_BUNDLE"}, []string{"REQUESTS_CA_BUNDLE=/op/requests.pem"})
	if len(names) != 1 || names[0] != "CURL_CA_BUNDLE" {
		t.Fatalf("names = %v, want CURL_CA_BUNDLE emitted for the reseat", names)
	}
	if len(synth) != 1 || synth[0] != "CURL_CA_BUNDLE=/op/requests.pem" {
		t.Fatalf("synth = %v, want CURL_CA_BUNDLE reseated to the forwarded bundle", synth)
	}

	// Both forwarded and set: each keeps its own approved value.
	names, synth = credentialMountTwinForwarding(credentialMountTrustBundlePairs,
		[]string{"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"},
		[]string{"REQUESTS_CA_BUNDLE=/op/requests.pem", "CURL_CA_BUNDLE=/op/curl.pem"})
	if len(names) != 0 || len(synth) != 0 {
		t.Fatalf("both members claimed must not reseat anything: names=%v synth=%v", names, synth)
	}

	// A forwarded-but-unset claim emits nothing (the claimed-empty semantics
	// the twins already carry) — and the exemption above still holds.
	names, synth = credentialMountTwinForwarding(credentialMountTrustBundlePairs,
		[]string{"CURL_CA_BUNDLE"}, []string{"UNRELATED=1"})
	if len(names) != 0 || len(synth) != 0 {
		t.Fatalf("a forwarded-but-unset claim must not reseat: names=%v synth=%v", names, synth)
	}
}
