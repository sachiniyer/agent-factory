package session

import (
	"slices"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// #7889: af's own provisioning execs inherit the container env where
// `-e SSL_CERT_FILE=` stands because `docker run` cannot unset an image ENV.
// The exec helpers prefix each script with an unset of the pinned
// path-valued trust names — the two whose measured empty read is a PATH —
// so no provisioning child (the clone's git, hooks, helpers) can inherit a
// defined-empty name an OpenSSL/rustls reader would load as zero roots.
// Each unset is conditioned on its SIBLING being empty: beside a forwarded
// sibling the pinned slot must keep defined-empty, since unset would
// restore the platform default and widen the operator's claimed trust
// (#7889, Codex 4722476016).
func TestTrustPathUnpinnedScriptUnsetsPinnedTrustNames(t *testing.T) {
	p := &dockerProvisioner{credentialMounts: []string{"mount"}}
	got := p.trustPathUnpinnedScript("git clone https://example.invalid/r.git")
	if !strings.Contains(got, `[ -n "$SSL_CERT_DIR" ] || unset SSL_CERT_FILE`) {
		t.Fatalf("SSL_CERT_FILE's unset must be conditioned on the dir slot being empty: %q", got)
	}
	if !strings.Contains(got, `[ -n "$SSL_CERT_FILE" ] || unset SSL_CERT_DIR`) {
		t.Fatalf("SSL_CERT_DIR's unset must be conditioned on the file slot being empty: %q", got)
	}
	// git's CA-root spellings join the unset list (#7891): CAINFO carries
	// no sibling guard — measured on git 2.43 an empty CAINFO poisons even
	// a valid CAPATH, so no defined-empty spelling is safe for it — while
	// CAPATH's empty read contributes nothing beside CAINFO, so it keeps
	// the sibling guard.
	for _, clause := range []string{
		"unset GIT_SSL_CAINFO", "unset GIT_PROXY_SSL_CAINFO",
		`[ -n "$GIT_SSL_CAINFO" ] || unset GIT_SSL_CAPATH`,
		`[ -n "$GIT_PROXY_SSL_CAINFO" ] || unset GIT_PROXY_SSL_CAPATH`,
	} {
		if !strings.Contains(got, clause) {
			t.Fatalf("the preamble must emit %q so af's git never sees the image's CA claim: %q", clause, got)
		}
	}
	// The presence-read names are unset unconditionally — a defined
	// `GIT_SSL_NO_VERIFY=` disables verification outright, so the image's
	// value must never survive to a provisioning child.
	for _, name := range []string{
		"GIT_SSL_NO_VERIFY", "GIT_SSL_CERT", "GIT_SSL_KEY",
		"GIT_HTTP_PROXY_AUTHMETHOD",
	} {
		if !strings.Contains(got, "unset "+name) {
			t.Fatalf("the preamble must unset %s — no defined value is inert for it: %q", name, got)
		}
	}
	// And the env-config injection names go too: an image GIT_CONFIG_COUNT
	// would animate whatever indexed pairs the image ships — including
	// http.sslVerify=false — for af's credentialed git.
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		if !strings.Contains(got, "unset "+name) {
			t.Fatalf("the preamble must unset %s — an image count animates the image's own config pairs: %q", name, got)
		}
	}
	if !strings.HasPrefix(got, "[ -n ") && !strings.HasPrefix(got, "unset ") {
		t.Fatalf("the unset clauses must lead the script so every later command sees them: %q", got)
	}
	if !strings.HasSuffix(got, "git clone https://example.invalid/r.git") {
		t.Fatalf("the operator's script must run verbatim after the preamble: %q", got)
	}
	// Everything else stays pinned defined-empty: the five falsy-safe
	// bundle spellings whose measured empty read is harmless (unset would
	// let a repo .env claim the name), and non-path pins where empty IS
	// the semantics (no proxy, no endpoint).
	for _, name := range []string{
		"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
		"CODEX_CA_CERTIFICATE", "HTTP_PROXY",
	} {
		if strings.Contains(got, "unset "+name) {
			t.Fatalf("%s keeps its defined-empty pin — it must not be unset: %q", name, got)
		}
	}
}

// An operator-forwarded trust name is claimed by session_env_passthrough
// and the clone needs that forwarded CA to reach a private-CA remote —
// the preamble never unsets a name the operator claimed; it may appear
// only as the sibling condition guarding a pinned slot's unset.
func TestTrustPathUnpinnedScriptKeepsForwardedValues(t *testing.T) {
	p := &dockerProvisioner{
		credentialMounts: []string{"mount"},
		spec:             ProvisionSpec{SessionEnvPassthrough: []string{"SSL_CERT_FILE", "GIT_SSL_NO_VERIFY"}},
	}
	got := p.trustPathUnpinnedScript("true")
	for _, name := range []string{"SSL_CERT_FILE", "GIT_SSL_NO_VERIFY"} {
		if strings.Contains(got, "unset "+name) {
			t.Fatalf("an operator-forwarded trust name must not be unset: %q", got)
		}
	}
	if !strings.Contains(got, "unset SSL_CERT_DIR") {
		t.Fatalf("an unforwarded trust name must be unset: %q", got)
	}
}

// Without a credential carried there are no claims to hold off — the
// script must be byte-identical so the preamble cannot perturb a plain
// session's image-supplied TLS config (a corporate-CA image is the
// operator's own choice there).
func TestTrustPathUnpinnedScriptNoPinsIsIdentity(t *testing.T) {
	p := &dockerProvisioner{}
	const script = "chmod +x /opt/af/bin/af"
	if got := p.trustPathUnpinnedScript(script); got != script {
		t.Fatalf("a session carrying no credential must pass its script through verbatim: %q", got)
	}
}

// Account-scoped sessions share the hole — the clone runs the same
// execSessionSh children and installs the same credential helper — so the
// preamble fires there too (#7891 widened to the shared surface).
func TestTrustPathUnpinnedScriptAppliesOnAccountSessions(t *testing.T) {
	p := &dockerProvisioner{spec: ProvisionSpec{
		Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: "/acct"},
	}}
	got := p.trustPathUnpinnedScript("git fetch origin")
	if !strings.Contains(got, "unset GIT_SSL_NO_VERIFY") {
		t.Fatalf("an account session's provisioning git must not see an image GIT_SSL_NO_VERIFY: %q", got)
	}
}

// Unforwarded, every trust spelling stays pinned (#7889 R2): the five
// bundle names are falsy-safe for their measured readers — node ignores
// NODE_EXTRA_CA_CERTS=”, requests falls through REQUESTS_CA_BUNDLE=” to
// CURL_CA_BUNDLE, curl falls from CURL_CA_BUNDLE=” to its compiled CAfile,
// codex treats CODEX_CA_CERTIFICATE=” as unset, grpc-js skips
// GRPC_DEFAULT_SSL_ROOTS_FILE_PATH=” — so defined-empty holds the
// repo/image claim off while staying inert, and SSL_CERT_FILE/SSL_CERT_DIR
// resolve to a real store at the shim.
func TestCredentialMountPinnedNames_AllTrustNamesStayPinned(t *testing.T) {
	for _, row := range []struct{ agent, name string }{
		// The shared routing names pin on every credential-mount agent.
		{"opencode", "SSL_CERT_FILE"}, {"opencode", "SSL_CERT_DIR"},
		{"opencode", "NODE_EXTRA_CA_CERTS"}, {"opencode", "REQUESTS_CA_BUNDLE"},
		{"opencode", "CURL_CA_BUNDLE"},
		// Per-agent tables and the gemini-bundle pin carry the rest.
		{"codex", "CODEX_CA_CERTIFICATE"},
		{"gemini", "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH"},
	} {
		if pinned := credentialMountPinnedNames(row.agent, []string{"OPERATOR_THING"}); !slices.Contains(pinned, row.name) {
			t.Fatalf("%s: unforwarded trust name %s must stay pinned", row.agent, row.name)
		}
	}
}

// A forwarded trust name exempts ONLY itself (#7889 R3 — no transitive
// exemption: the falsy-safe siblings' defined-empty pins are inert for
// their readers, so nothing needs reseating to keep the operator's CA
// ahead).
func TestCredentialMountPinnedNames_ForwardedTrustNameExemptsOnlyItself(t *testing.T) {
	pinned := credentialMountPinnedNames("opencode", []string{"SSL_CERT_FILE"})
	if slices.Contains(pinned, "SSL_CERT_FILE") {
		t.Fatal("the forwarded name itself must leave the pin set")
	}
	for _, name := range []string{
		"SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE",
	} {
		if !slices.Contains(pinned, name) {
			t.Fatalf("%s must stay pinned when only SSL_CERT_FILE is forwarded", name)
		}
	}
}

// #7889 R2: a forwarded operator CA must WIN its reader chain — no af-set
// value may sit ahead of it. Because the falsy-safe spellings keep their
// defined-empty pin, each name a reader consults FIRST reads falsy and the
// reader falls through to the operator's value (or its compiled default).
// The table is the audit: each row names a reader chain and the names that
// must stay defined-empty-pinned for the forwarded claim to win.
func TestForwardedOperatorCAWinsEveryReaderChain(t *testing.T) {
	chains := []struct {
		reader   string
		agent    string   // which agent's pin table carries the names
		forward  string   // the operator's claim through session_env_passthrough
		falsyPin []string // consulted first — must stay pinned so empty falls through
	}{
		// curl: CURL_CA_BUNDLE beats SSL_CERT_FILE for --cacert, and '' is
		// skipped (compiled CAfile/SSL_CERT_FILE next) → the file wins.
		{"curl", "opencode", "SSL_CERT_FILE", []string{"CURL_CA_BUNDLE"}},
		// requests: `REQUESTS_CA_BUNDLE or CURL_CA_BUNDLE or verify` —
		// '' is falsy at each step → a forwarded member wins.
		{"requests", "opencode", "CURL_CA_BUNDLE", []string{"REQUESTS_CA_BUNDLE"}},
		{"requests", "opencode", "REQUESTS_CA_BUNDLE", nil},
		// grpc-js skips an empty GRPC_DEFAULT_SSL_ROOTS_FILE_PATH and
		// codex's custom_ca.rs treats CODEX_CA_CERTIFICATE='' as unset —
		// forwarded, each name is its reader's only override.
		{"grpc-js", "gemini", "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", nil},
		{"codex", "codex", "CODEX_CA_CERTIFICATE", nil},
	}
	for _, c := range chains {
		pinned := credentialMountPinnedNames(c.agent, []string{c.forward})
		if slices.Contains(pinned, c.forward) {
			t.Fatalf("%s chain: forwarded %s must leave the pin set", c.reader, c.forward)
		}
		for _, name := range c.falsyPin {
			if !slices.Contains(pinned, name) {
				t.Fatalf("%s chain: %s must stay pinned defined-empty so its empty read falls through to %s", c.reader, name, c.forward)
			}
		}
	}
}
