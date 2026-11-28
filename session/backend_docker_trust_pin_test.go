package session

import (
	"os"
	"os/exec"
	"slices"
	"strconv"
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

	// Forwarded XDG_CONFIG_HOME with the global slot unclaimed: on
	// credential-mount the claim is undeliverable (af re-asserts
	// XDG_CONFIG_HOME to the session home after run_args), so the name is
	// dropped from the claim set entirely — af's file stays the global and
	// the redirect would otherwise point at the image-controlled home's
	// git/config (#7891 F15). On the ACCOUNT path the forward does reach
	// the container, so the claim holds there: GIT_CONFIG_GLOBAL redirects
	// at the claimed file — an export replaces the XDG global too, so af's
	// own file would silently shadow it (Codex on #7891 N1).
	t.Setenv("XDG_CONFIG_HOME", "/operator/xdg")
	got = newP("XDG_CONFIG_HOME").trustPathUnpinnedScript("true")
	for _, clause := range []string{
		"export GIT_CONFIG_GLOBAL='/root/.af-gitconfig'", "unset XDG_CONFIG_HOME",
	} {
		if !strings.Contains(got, clause) {
			t.Fatalf("an undeliverable XDG claim must not exempt its slot — %q must still apply: %q", clause, got)
		}
	}
	if strings.Contains(got, "${XDG_CONFIG_HOME%/}/git/config") {
		t.Fatalf("the XDG redirect must never point at the image-controlled home: %q", got)
	}
	got = (&dockerProvisioner{
		spec: ProvisionSpec{
			Account:               sessionenv.Account{Agent: "gemini", Dir: "/acct"},
			SessionEnvPassthrough: []string{"XDG_CONFIG_HOME"},
		},
	}).trustPathUnpinnedScript("true")
	if !strings.Contains(got, `export GIT_CONFIG_GLOBAL="${XDG_CONFIG_HOME%/}/git/config"`) {
		t.Fatalf("an XDG-only claim must redirect af's global at the claimed XDG file where the forward is deliverable: %q", got)
	}
	os.Unsetenv("XDG_CONFIG_HOME")

	// Every DELIVERABLE route name claimed on credential-mount: the XDG
	// slot can never be claimed there (the forward never reaches the
	// container), so its unset stands while the other slots exempt.
	t.Setenv("GIT_CONFIG_GLOBAL", "/operator/global.gitconfig")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", "/operator/xdg")
	got = newP("GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME").
		trustPathUnpinnedScript("true")
	for _, clause := range []string{
		"export GIT_CONFIG_NOSYSTEM", "export GIT_CONFIG_GLOBAL", "af-gitconfig",
		"unset GIT_CONFIG_SYSTEM", "unset GIT_CONFIG_NOSYSTEM",
	} {
		if strings.Contains(got, clause) {
			t.Fatalf("claiming the deliverable route names exempts their slots — %q must not appear: %q", clause, got)
		}
	}
	if !strings.Contains(got, "unset XDG_CONFIG_HOME") {
		t.Fatalf("XDG_CONFIG_HOME cannot be claimed on credential-mount — its unset must stand: %q", got)
	}

	// On the account path every route name claimed lifts the lockdown
	// whole — XDG is deliverable there, so the ambient environment IS the
	// operator's claim.
	got = (&dockerProvisioner{
		spec: ProvisionSpec{
			Account:               sessionenv.Account{Agent: "gemini", Dir: "/acct"},
			SessionEnvPassthrough: afGitConfigRouteNames,
		},
	}).trustPathUnpinnedScript("true")
	for _, clause := range []string{
		"export GIT_CONFIG_NOSYSTEM", "export GIT_CONFIG_GLOBAL", "af-gitconfig",
		"unset GIT_CONFIG_SYSTEM", "unset XDG_CONFIG_HOME", "unset GIT_CONFIG_NOSYSTEM",
	} {
		if strings.Contains(got, clause) {
			t.Fatalf("claiming every route name lifts the lockdown on the account path — %q must not appear: %q", clause, got)
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
// value may sit ahead of it. This is proven through the exec shim itself,
// not pin-set membership (#7895): each row re-execs this test binary with
// the pane's real argv — os.Args[1] = sessionenv.ExecMarker dispatches
// through sessionenv.HandleInternalExec (installed by this package's
// TestMain, exactly as main.go installs it) into the real execInvocation —
// carrying the operator's forwarded value in the extras slot the way
// session_env_passthrough delivers it and the REAL
// credentialMountPinnedNames output as the pin list. The environment the
// shim's final syscall.Exec installs is what the agent inherits, so the
// assertion walks the chain in the order the reader consults it and
// requires the forwarded name to be the first non-empty read.
//
// What this catches that membership never could: the falsy-safe spellings
// must stay defined-empty-pinned PRECISELY because their empty read falls
// through — a mutation re-adding CURL_CA_BUNDLE,
// GRPC_DEFAULT_SSL_ROOTS_FILE_PATH or CODEX_CA_CERTIFICATE to
// trustPathPinCandidates resolves that name to a platform bundle AHEAD of
// the forwarded member in its chain and the row fails (#7890's R2
// regression).
func TestForwardedOperatorCAWinsEveryReaderChain(t *testing.T) {
	// The reader chains, in consult order. Each row forwards one member and
	// asserts it is the chain's first non-empty name in the shim's env; the
	// other members ride the pin list, so the row also proves each stays a
	// falsy-safe defined-empty rather than resolving into the chain.
	chains := []struct {
		reader string
		agent  string // whose credential-mount pin table applies
		names  []string
	}{
		// curl consults CURL_CA_BUNDLE before SSL_CERT_FILE for --cacert;
		// an empty CURL_CA_BUNDLE is skipped (compiled CAfile next).
		{"curl", "opencode", []string{"CURL_CA_BUNDLE", "SSL_CERT_FILE"}},
		// requests: `REQUESTS_CA_BUNDLE or CURL_CA_BUNDLE or verify`.
		{"requests", "opencode", []string{"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"}},
		// grpc-js's GRPC_DEFAULT_SSL_ROOTS_FILE_PATH replaces node's roots
		// where NODE_EXTRA_CA_CERTS was meant only to add.
		{"grpc-js", "gemini", []string{"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", "NODE_EXTRA_CA_CERTS"}},
		// codex's custom_ca.rs reads CODEX_CA_CERTIFICATE before rustls's
		// SSL_CERT_FILE.
		{"codex", "codex", []string{"CODEX_CA_CERTIFICATE", "SSL_CERT_FILE"}},
		// OpenSSL and rustls-native-certs both load the file slot before
		// the hashed dir.
		{"OpenSSL", "opencode", []string{"SSL_CERT_FILE", "SSL_CERT_DIR"}},
		{"rustls", "opencode", []string{"SSL_CERT_FILE", "SSL_CERT_DIR"}},
	}
	const operatorCA = "/operator/corp-ca.pem"
	for _, c := range chains {
		for _, forward := range c.names {
			pins := credentialMountPinnedNames(c.agent, []string{forward})
			if slices.Contains(pins, forward) {
				t.Fatalf("%s chain: forwarded %s must leave the pin set", c.reader, forward)
			}
			// The pane argv WrapCommand emits: marker, agent, the extras
			// count and names (the operator's claim), the pins count and
			// names, then the command — env dumps the shim's final
			// environment.
			argv := []string{sessionenv.ExecMarker, c.agent, "1", forward,
				strconv.Itoa(len(pins))}
			argv = append(argv, pins...)
			argv = append(argv, "env")
			// The container env the shim reads: the forwarded value plus
			// every pinned name defined-empty, exactly as `docker run
			// -e NAME=` delivers them — and nothing else: TMUX_PANE
			// absent keeps stampPaneRootPid off the host's tmux, and the
			// unattributed `env` command leaves only common and extras
			// names through the filter, so the forwarded value survives
			// solely on the operator's claim.
			env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", forward + "=" + operatorCA}
			for _, pin := range pins {
				env = append(env, pin+"=")
			}
			cmd := exec.Command(os.Args[0], argv...)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s forwarding %s: the exec shim refused: %v: %s", c.reader, forward, err, out)
			}
			got := make(map[string]string)
			for _, line := range strings.Split(string(out), "\n") {
				if name, value, ok := strings.Cut(line, "="); ok {
					got[name] = value
				}
			}
			first := ""
			for _, name := range c.names {
				if got[name] != "" {
					first = name
					break
				}
			}
			if first != forward {
				t.Fatalf("%s chain %v forwarding %s: the reader's first non-empty name must be the operator's claim; the shim's env gives %s (env %v)", c.reader, c.names, forward, first, got)
			}
		}
	}
}
