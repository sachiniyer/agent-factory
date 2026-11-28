package session

import (
	"os"
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
	// Every git trust name is UNSET, never resolved (#7891 R3): a resolved
	// env CAINFO outranks a config-file CA the operator forwarded, and a
	// resolved bundle widens a dir-only forward — the image's value must
	// never survive to af's credentialed git.
	for _, name := range sessionenv.TrustStripNames() {
		if !strings.Contains(got, "unset "+name) {
			t.Fatalf("the preamble must unset %s — no defined or resolved value is safe for it: %q", name, got)
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
	// The config lockdown (#7891 R1): af's own git reads only files af
	// created — system config off, global pointed at the af-owned file,
	// both redirect names dropped.
	for _, clause := range []string{
		"unset GIT_CONFIG_SYSTEM", "unset XDG_CONFIG_HOME",
		"export GIT_CONFIG_NOSYSTEM=1", "export GIT_CONFIG_GLOBAL='/root/.af-gitconfig'",
	} {
		if !strings.Contains(got, clause) {
			t.Fatalf("the preamble must own every unclaimed config slot — missing %q: %q", clause, got)
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
// the preamble never unsets a name carrying the operator's forwarded
// VALUE; it may appear only as the sibling condition guarding a pinned
// slot's unset (#7891 R6: a passthrough claim the daemon env lacks emits
// no -e, so exempting the claim would let the image's own ENV through —
// the exemption keys on forwardedEnvNames, not the raw list).
func TestTrustPathUnpinnedScriptKeepsForwardedValues(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "/operator/ca.pem")
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	if v, ok := os.LookupEnv("GIT_SSL_CAPATH"); ok {
		t.Cleanup(func() { os.Setenv("GIT_SSL_CAPATH", v) })
	}
	os.Unsetenv("GIT_SSL_CAPATH")
	p := &dockerProvisioner{
		credentialMounts: []string{"mount"},
		spec: ProvisionSpec{SessionEnvPassthrough: []string{
			"SSL_CERT_FILE", "GIT_SSL_NO_VERIFY", "GIT_SSL_CAPATH",
		}},
	}
	got := p.trustPathUnpinnedScript("true")
	for _, name := range []string{"SSL_CERT_FILE", "GIT_SSL_NO_VERIFY"} {
		if strings.Contains(got, "unset "+name) {
			t.Fatalf("a trust name forwarded WITH a host value must not be unset: %q", got)
		}
	}
	if !strings.Contains(got, "unset GIT_SSL_CAPATH") {
		t.Fatalf("a claimed-but-hostless passthrough name emits no -e — the image's ENV would survive, so the unset must stand: %q", got)
	}
	if !strings.Contains(got, "unset SSL_CERT_DIR") {
		t.Fatalf("an unforwarded trust name must be unset: %q", got)
	}
}

// A forwarded git-config-route name exempts ONLY its own slot (#7891 N1,
// the re-review's per-slot rule): forwarding GIT_CONFIG_GLOBAL keeps the
// operator's file but leaves system config off and the XDG redirect
// dropped; forwarding GIT_CONFIG_NOSYSTEM keeps the operator's flag while
// the af-owned global still applies. Only claiming EVERY route name lifts
// the lockdown whole.
func TestTrustPathUnpinnedScriptLockdownIsPerSlot(t *testing.T) {
	newP := func(names ...string) *dockerProvisioner {
		return &dockerProvisioner{
			credentialMounts: []string{"mount"},
			spec:             ProvisionSpec{SessionEnvPassthrough: names},
		}
	}

	// Forwarded GIT_CONFIG_GLOBAL: only the global slot is claimed.
	t.Setenv("GIT_CONFIG_GLOBAL", "/operator/global.gitconfig")
	got := newP("GIT_CONFIG_GLOBAL").trustPathUnpinnedScript("true")
	for _, clause := range []string{
		"export GIT_CONFIG_NOSYSTEM=1", "unset GIT_CONFIG_SYSTEM", "unset XDG_CONFIG_HOME",
	} {
		if !strings.Contains(got, clause) {
			t.Fatalf("forwarding GIT_CONFIG_GLOBAL claims only the global slot — %q must still apply: %q", clause, got)
		}
	}
	if strings.Contains(got, "export GIT_CONFIG_GLOBAL=") {
		t.Fatalf("the claimed global slot must keep the operator's file — af must not export its own: %q", got)
	}

	// Forwarded GIT_CONFIG_NOSYSTEM: the flag is the operator's, every file
	// slot stays af's.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	got = newP("GIT_CONFIG_NOSYSTEM").trustPathUnpinnedScript("true")
	for _, clause := range []string{
		"export GIT_CONFIG_GLOBAL='/root/.af-gitconfig'",
		"unset GIT_CONFIG_SYSTEM", "unset XDG_CONFIG_HOME",
	} {
		if !strings.Contains(got, clause) {
			t.Fatalf("forwarding GIT_CONFIG_NOSYSTEM claims only the flag — %q must still apply: %q", clause, got)
		}
	}
	for _, clause := range []string{"export GIT_CONFIG_NOSYSTEM", "unset GIT_CONFIG_NOSYSTEM"} {
		if strings.Contains(got, clause) {
			t.Fatalf("the claimed NOSYSTEM slot must keep the operator's value — %q must not appear: %q", clause, got)
		}
	}

	// Forwarded GIT_CONFIG_SYSTEM: the claimed file would still lose to an
	// ambient or pinned NOSYSTEM, so that name is dropped rather than af's
	// =1 asserted over the operator's claim.
	t.Setenv("GIT_CONFIG_SYSTEM", "/operator/gitconfig")
	got = newP("GIT_CONFIG_SYSTEM").trustPathUnpinnedScript("true")
	if !strings.Contains(got, "export GIT_CONFIG_GLOBAL='/root/.af-gitconfig'") {
		t.Fatalf("the unclaimed global slot stays af's when SYSTEM is claimed: %q", got)
	}
	for _, clause := range []string{
		"unset GIT_CONFIG_SYSTEM", "export GIT_CONFIG_NOSYSTEM=1",
	} {
		if strings.Contains(got, clause) {
			t.Fatalf("the claimed system slot must survive — %q would defeat it: %q", clause, got)
		}
	}
	if !strings.Contains(got, "unset GIT_CONFIG_NOSYSTEM") {
		t.Fatalf("an ambient NOSYSTEM must be dropped so the claimed system file is read: %q", got)
	}

	// Forwarded XDG_CONFIG_HOME with the global slot unclaimed: the claimed
	// file only reaches git if af's GIT_CONFIG_GLOBAL redirect points AT it
	// — a GIT_CONFIG_GLOBAL export replaces the XDG global too, so af's own
	// file would silently shadow the operator's claim (Codex on #7891 N1).
	// The image's ~/.gitconfig stays shadowed either way.
	t.Setenv("XDG_CONFIG_HOME", "/operator/xdg")
	got = newP("XDG_CONFIG_HOME").trustPathUnpinnedScript("true")
	if !strings.Contains(got, `export GIT_CONFIG_GLOBAL="${XDG_CONFIG_HOME%/}/git/config"`) {
		t.Fatalf("an XDG-only claim must redirect af's global at the claimed XDG file: %q", got)
	}
	for _, clause := range []string{"af-gitconfig", "unset XDG_CONFIG_HOME"} {
		if strings.Contains(got, clause) {
			t.Fatalf("the claimed XDG slot must reach git — %q would shadow or drop it: %q", clause, got)
		}
	}
	for _, clause := range []string{"export GIT_CONFIG_NOSYSTEM=1", "unset GIT_CONFIG_SYSTEM"} {
		if !strings.Contains(got, clause) {
			t.Fatalf("the unclaimed slots stay af's under an XDG claim — %q must still apply: %q", clause, got)
		}
	}
	os.Unsetenv("XDG_CONFIG_HOME")

	// Every route name claimed: the lockdown lifts whole — the ambient
	// environment IS the operator's claim.
	t.Setenv("GIT_CONFIG_GLOBAL", "/operator/global.gitconfig")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", "/operator/xdg")
	got = newP("GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME").
		trustPathUnpinnedScript("true")
	for _, clause := range []string{
		"export GIT_CONFIG_NOSYSTEM", "export GIT_CONFIG_GLOBAL", "af-gitconfig",
		"unset GIT_CONFIG_SYSTEM", "unset XDG_CONFIG_HOME", "unset GIT_CONFIG_NOSYSTEM",
	} {
		if strings.Contains(got, clause) {
			t.Fatalf("claiming every route name lifts the lockdown — %q must not appear: %q", clause, got)
		}
	}

	// A claimed route name that emits no -e (hostless) does NOT claim: the
	// lockdown still applies so af's git cannot read the image's files.
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME"} {
		os.Unsetenv(name)
	}
	got = newP("GIT_CONFIG_GLOBAL").trustPathUnpinnedScript("true")
	if !strings.Contains(got, "export GIT_CONFIG_GLOBAL='/root/.af-gitconfig'") {
		t.Fatalf("a hostless config-route claim must not exempt even its own slot: %q", got)
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
