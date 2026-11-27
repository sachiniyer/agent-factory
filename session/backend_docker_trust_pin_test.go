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

// The CA-bundle spellings form a precedence CHAIN across readers: Requests
// merges `REQUESTS_CA_BUNDLE or CURL_CA_BUNDLE or verify` and curl prefers
// `CURL_CA_BUNDLE` over `SSL_CERT_FILE` for --cacert — so a member af sets,
// even to a real platform bundle, outranks a lower member the operator
// forwarded for a private CA (#7889 review). Forwarding ANY member must
// exempt the WHOLE chain from the pin, exactly like the proxy case twins.
func TestCredentialMountPinnedNames_TrustBundleChainExemptsAll(t *testing.T) {
	for _, forwarded := range credentialMountTrustBundleGroup {
		pinned := credentialMountPinnedNames("opencode", []string{forwarded})
		for _, member := range credentialMountTrustBundleGroup {
			if slices.Contains(pinned, member) {
				t.Fatalf("forwarding %s must exempt chain member %s from the pin", forwarded, member)
			}
		}
	}
	// Unforwarded, all three stay pinned — the resolved-path pin only
	// matters when the operator did not claim the chain. SSL_CERT_DIR is
	// no chain member (no reader falls back to it from a bundle file) and
	// stays pinned regardless.
	pinned := credentialMountPinnedNames("opencode", []string{"OPERATOR_THING"})
	for _, member := range credentialMountTrustBundleGroup {
		if !slices.Contains(pinned, member) {
			t.Fatalf("unforwarded trust-bundle name %s must stay pinned", member)
		}
	}
	if !slices.Contains(pinned, "SSL_CERT_DIR") {
		t.Fatal("SSL_CERT_DIR must stay pinned — it is not a chain member")
	}
}

// The unforwarded members cannot simply drop the pin — their names are in
// the inner filter's always-admitted set, so an image value for them would
// reach the agent. They are reseated to the operator's forwarded bundle
// through the chain machinery instead (bare `-e NAME` + synthesized NAME=V
// in the docker client env, so the value never enters argv).
func TestCredentialMountTrustBundleForwarding_ReseatsChainToForwardedBundle(t *testing.T) {
	// Forwarding CURL_CA_BUNDLE: the operator's bundle must outrank the
	// resolved platform path for Requests (REQUESTS sits ahead of CURL in
	// its chain) AND for curl over nothing — both siblings reseat.
	names, synth := credentialMountChainForwarding(credentialMountTrustBundleGroup,
		[]string{"CURL_CA_BUNDLE"}, []string{"CURL_CA_BUNDLE=/operator/private-ca.pem"})
	if !slices.Equal(names, []string{"REQUESTS_CA_BUNDLE", "SSL_CERT_FILE"}) {
		t.Fatalf("names = %v, want both unclaimed members emitted for the reseat", names)
	}
	if !slices.Equal(synth, []string{
		"REQUESTS_CA_BUNDLE=/operator/private-ca.pem",
		"SSL_CERT_FILE=/operator/private-ca.pem",
	}) {
		t.Fatalf("synth = %v, want both members reseated to the forwarded bundle", synth)
	}

	// Forwarding SSL_CERT_FILE (the finding's exact case): curl prefers
	// CURL_CA_BUNDLE, so CURL must reseat or the platform pin outranks the
	// operator's CA; REQUESTS reseats so Requests reaches it too.
	names, synth = credentialMountChainForwarding(credentialMountTrustBundleGroup,
		[]string{"SSL_CERT_FILE"}, []string{"SSL_CERT_FILE=/op/openssl-ca.pem"})
	if !slices.Equal(names, []string{"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"}) {
		t.Fatalf("names = %v, want REQUESTS and CURL emitted for the reseat", names)
	}
	if !slices.Equal(synth, []string{
		"REQUESTS_CA_BUNDLE=/op/openssl-ca.pem",
		"CURL_CA_BUNDLE=/op/openssl-ca.pem",
	}) {
		t.Fatalf("synth = %v, want the whole chain reseated to the forwarded bundle", synth)
	}

	// All members forwarded and set: each keeps its own approved value.
	names, synth = credentialMountChainForwarding(credentialMountTrustBundleGroup,
		credentialMountTrustBundleGroup,
		[]string{"REQUESTS_CA_BUNDLE=/op/r.pem", "CURL_CA_BUNDLE=/op/c.pem", "SSL_CERT_FILE=/op/s.pem"})
	if len(names) != 0 || len(synth) != 0 {
		t.Fatalf("a fully claimed chain must not reseat anything: names=%v synth=%v", names, synth)
	}

	// A forwarded-but-unset claim emits nothing (the claimed-empty semantics
	// the twins already carry) — and the exemption above still holds.
	names, synth = credentialMountChainForwarding(credentialMountTrustBundleGroup,
		[]string{"CURL_CA_BUNDLE"}, []string{"UNRELATED=1"})
	if len(names) != 0 || len(synth) != 0 {
		t.Fatalf("a forwarded-but-unset claim must not reseat: names=%v synth=%v", names, synth)
	}
}
