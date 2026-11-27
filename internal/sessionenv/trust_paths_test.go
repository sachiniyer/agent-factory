package sessionenv

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"testing"
	"time"
)

// #7889: a path-valued trust pin must be inert for every reader it reaches.
// Defined-empty for SSL_CERT_FILE/SSL_CERT_DIR is a PATH their TLS stacks
// load — OpenSSL and rustls-native-certs read the empty value and drop every
// platform root — so the exec shim re-asserts each name to a real bundle
// location instead, and the agent-server startup neutralizes them for every
// non-pane child. These tables run the resolution and both boundaries that
// apply it; on master every row emitted NAME= and zeroed the container's
// trust store.

// stubTrustPathStat swaps the resolver's filesystem probe for a fixture that
// reports exactly the listed paths as existing (dirs marked by a trailing
// slash), so each row is deterministic on any host.
func stubTrustPathStat(t *testing.T, existing ...string) {
	t.Helper()
	present := make(map[string]bool, len(existing))
	for _, p := range existing {
		isDir := len(p) > 1 && p[len(p)-1] == '/'
		if isDir {
			p = p[:len(p)-1]
		}
		present[p] = true
		if isDir {
			present[p+"/"] = true
		}
	}
	previous := trustPathStat
	trustPathStat = func(name string) (os.FileInfo, error) {
		isDir := present[name+"/"]
		if !present[name] {
			return nil, fs.ErrNotExist
		}
		return fakeTrustPathInfo{dir: isDir}, nil
	}
	t.Cleanup(func() { trustPathStat = previous })
}

// fakeTrustPathInfo is the os.FileInfo the stat fixture returns — only IsDir
// is consulted by the resolver.
type fakeTrustPathInfo struct{ dir bool }

func (f fakeTrustPathInfo) Name() string      { return "fixture" }
func (f fakeTrustPathInfo) Size() int64       { return 0 }
func (f fakeTrustPathInfo) Mode() fs.FileMode { return 0 }
func (f fakeTrustPathInfo) ModTime() time.Time {
	return time.Time{}
}
func (f fakeTrustPathInfo) IsDir() bool      { return f.dir }
func (f fakeTrustPathInfo) Sys() interface{} { return nil }

// Every path-valued pin name resolves to the first candidate that exists in
// the container, in the table's preference order. The bundle-file names take
// a file; SSL_CERT_DIR takes the hashed directory.
func TestResolveTrustPathPinPrefersFirstExistingCandidate(t *testing.T) {
	for _, name := range TrustPathPinNames() {
		candidates := trustPathPinCandidates[name]
		if len(candidates) < 2 {
			t.Fatalf("%s must list at least two candidates for the preference check", name)
		}
		// Mark every candidate except the first as existing: for the file
		// names the second entry wins; for the CAPATH-class dir names the
		// dir spelling.
		var existing []string
		dirWanted := slices.Equal(candidates, caBundleDirCandidates)
		for _, c := range candidates[1:] {
			if dirWanted {
				existing = append(existing, c+"/")
			} else {
				existing = append(existing, c)
			}
		}
		t.Run(name, func(t *testing.T) {
			stubTrustPathStat(t, existing...)
			entry, ok := resolveTrustPathPin(name)
			if !ok {
				t.Fatalf("%s must be a recognized path-valued pin", name)
			}
			want := name + "=" + candidates[1]
			if entry != want {
				t.Fatalf("resolveTrustPathPin(%s) = %q, want %q (first existing candidate)", name, entry, want)
			}
		})
	}
}

// The kind must match the reader: SSL_CERT_DIR wants a directory, the bundle
// names want a file. A candidate of the wrong kind is skipped, not asserted.
func TestResolveTrustPathPinRequiresMatchingKind(t *testing.T) {
	// A FILE at a SSL_CERT_DIR location is not a hashed trust dir; the
	// resolver falls through to defined-empty rather than claim it.
	stubTrustPathStat(t, "/etc/ssl/certs", "/etc/pki/tls/certs")
	entry, ok := resolveTrustPathPin("SSL_CERT_DIR")
	if !ok || entry != "SSL_CERT_DIR=" {
		t.Fatalf("SSL_CERT_DIR with only files at dir candidates = %q (ok %v), want the defined-empty fallback", entry, ok)
	}
	// And a DIRECTORY at a bundle-file location is not a PEM bundle.
	entry, ok = resolveTrustPathPin("SSL_CERT_FILE")
	if !ok || entry != "SSL_CERT_FILE=" {
		t.Fatalf("SSL_CERT_FILE with only dirs at file candidates = %q (ok %v), want the defined-empty fallback", entry, ok)
	}
}

// On a store-less image the pin stays defined-and-empty rather than going
// unset: unset is claimable by an agent .env loader, and empty buys the same
// zero CAs on an image that has no bundle at any known location anyway. The
// defined-empty fallback is the fail-closed residue, not a regression.
func TestResolveTrustPathPinKeepsDefinedEmptyWithoutAStore(t *testing.T) {
	stubTrustPathStat(t)
	for _, name := range TrustPathPinNames() {
		entry, ok := resolveTrustPathPin(name)
		if !ok {
			t.Fatalf("%s must be a recognized path-valued pin", name)
		}
		if entry != name+"=" {
			t.Fatalf("%s with no existing candidate = %q, want defined-empty (the name must stay claimed)", name, entry)
		}
	}
}

// A name outside the path-valued class is not resolved — proxies, endpoint
// selectors, the empty-equals-unset names, and above all the five falsy-safe
// bundle spellings whose measured empty read is harmless (#7889 review:
// NODE_EXTRA_CA_CERTS node-ignored, REQUESTS_CA_BUNDLE falls through to
// CURL_CA_BUNDLE, CURL_CA_BUNDLE falls to curl's compiled CAfile,
// CODEX_CA_CERTIFICATE reads empty-as-unset, GRPC_DEFAULT_SSL_ROOTS_FILE_PATH
// is skipped empty). Resolving them would let an af-set platform path
// OUTRANK an operator-forwarded sibling in the reader chains.
func TestResolveTrustPathPinLeavesNonPathNamesAlone(t *testing.T) {
	for _, name := range []string{
		"NODE_EXTRA_CA_CERTS",
		"REQUESTS_CA_BUNDLE",
		"CURL_CA_BUNDLE",
		"CODEX_CA_CERTIFICATE",
		"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH",
		"HTTP_PROXY", "HTTPS_PROXY", "GH_HOST", "AF_DAEMON_TOKEN", "NOT_A_NAME",
	} {
		if _, ok := resolveTrustPathPin(name); ok {
			t.Fatalf("%s must not resolve as a path-valued pin", name)
		}
	}
}

// The table the session side consumes: the provisioning exec preamble unsets
// exactly these names, so the set must stay the measured path-valued class —
// the names whose empty value a TLS stack reads as a path (#7889 review:
// the five falsy-safe bundle spellings keep master's defined-empty pin).
// git's own CA names are NOT here (#7891 R3): they are strip names, unset
// at every hop — a resolved CAINFO outranks a config-file CA the operator
// forwarded, and a resolved bundle widens a dir-only forward.
func TestTrustPathPinNamesListsTheAuditedClass(t *testing.T) {
	want := []string{
		"SSL_CERT_DIR",
		"SSL_CERT_FILE",
	}
	got := TrustPathPinNames()
	if !slices.Equal(got, want) {
		t.Fatalf("TrustPathPinNames() = %v, want the sorted audited set %v", got, want)
	}
}

// #7891 R7: the strip set is exactly what `strings git-remote-https` shows
// git 2.43 reading — no documented-but-unread names, and every reader it
// does have. These names are unset at the provisioning preamble and simply
// absent at every filtered hop; they are never pinned and never resolved.
func TestTrustStripNamesListsGitsRealEnvSet(t *testing.T) {
	want := []string{
		"GIT_HTTP_PROXY_AUTHMETHOD",
		"GIT_PROXY_SSL_CAINFO",
		"GIT_PROXY_SSL_CERT",
		"GIT_PROXY_SSL_CERT_PASSWORD_PROTECTED",
		"GIT_PROXY_SSL_KEY",
		"GIT_SSL_CAINFO",
		"GIT_SSL_CAPATH",
		"GIT_SSL_CERT",
		"GIT_SSL_CERT_PASSWORD_PROTECTED",
		"GIT_SSL_CERT_TYPE",
		"GIT_SSL_CIPHER_LIST",
		"GIT_SSL_KEY",
		"GIT_SSL_KEY_TYPE",
		"GIT_SSL_NO_VERIFY",
		"GIT_SSL_VERSION",
		"GIT_TEMPLATE_DIR",
	}
	got := TrustStripNames()
	if !slices.Equal(got, want) {
		t.Fatalf("TrustStripNames() = %v, want git's real env set %v", got, want)
	}
	for _, name := range want {
		if _, ok := trustPathPinCandidates[name]; ok {
			t.Fatalf("%s must never be a resolved pin — unset is the only spelling (#7891 R3)", name)
		}
	}
}

// The boundary claim of #7889: at the exec shim — the last environment af
// controls before the agent — a path-valued trust pin reaches the exec'd
// child defined with a NON-EMPTY path that exists, never defined-empty and
// never the value an intervening program (tmux's default-shell chain, the
// container env itself) re-set. A non-path pin beside it keeps its
// defined-empty shape. On master this row emitted SSL_CERT_FILE= and the
// agent's OpenSSL loaded zero CAs.
func TestExecInvocationResolvesTrustPathPins(t *testing.T) {
	stubTrustPathStat(t, "/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/certs/")
	t.Setenv("SSL_CERT_FILE", "attacker-or-image-value")
	pathDir := t.TempDir()
	writeExecutableFixture(t, pathDir, "codex")
	t.Setenv("PATH", pathDir)
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	// [agent, extrasCount, extras..., pinsCount, pins..., command]
	err := execInvocation([]string{"codex", "0", "4", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "REQUESTS_CA_BUNDLE", "codex"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("execInvocation() error = %v, want test sentinel", err)
	}
	if got, _ := envValue(gotEnvironment, "SSL_CERT_FILE"); got != "/etc/ssl/certs/ca-certificates.crt" {
		t.Fatalf("SSL_CERT_FILE = %q: the shim must hand the agent a real bundle path, never defined-empty or the re-set value", got)
	}
	if got, _ := envValue(gotEnvironment, "SSL_CERT_DIR"); got != "/etc/ssl/certs" {
		t.Fatalf("SSL_CERT_DIR = %q: the shim must hand the agent the hashed trust directory", got)
	}
	if got, ok := envValue(gotEnvironment, "HTTP_PROXY"); !ok || got != "" {
		t.Fatalf("HTTP_PROXY = %q (present %v): a non-path pin keeps its defined-empty shape", got, ok)
	}
	// The falsy-safe bundle spellings keep master's defined-empty pin:
	// resolving them would shadow an operator-forwarded sibling in the
	// reader chains (requests falls through REQUESTS_CA_BUNDLE='' to
	// CURL_CA_BUNDLE; a resolved path would win instead).
	if got, ok := envValue(gotEnvironment, "REQUESTS_CA_BUNDLE"); !ok || got != "" {
		t.Fatalf("REQUESTS_CA_BUNDLE = %q (present %v): a falsy-safe pin keeps its defined-empty shape", got, ok)
	}
	if slices.Contains(gotEnvironment, "SSL_CERT_FILE=attacker-or-image-value") {
		t.Fatal("a value re-set between docker run and the exec survived the trust-path pin")
	}
}

// #7889 R5: a forwarded SSL_CERT_FILE claims the file slot for the
// operator's CA — a resolved hashed dir beside it would hand rustls the
// image's roots too, where the dir unset trusts only the operator's file.
// The shim keeps the dir pin defined-empty in that case.
func TestExecInvocationKeepsDirPinWhenCertFileForwarded(t *testing.T) {
	stubTrustPathStat(t, "/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/certs/")
	t.Setenv("SSL_CERT_FILE", "/operator/corporate-ca.pem")
	pathDir := t.TempDir()
	writeExecutableFixture(t, pathDir, "codex")
	t.Setenv("PATH", pathDir)
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	// SSL_CERT_FILE rides extras (the operator's claim); SSL_CERT_DIR is a pin.
	if err := execInvocation([]string{"codex", "1", "SSL_CERT_FILE", "1", "SSL_CERT_DIR", "codex"}); !errors.Is(err, wantErr) {
		t.Fatalf("execInvocation() error = %v, want test sentinel", err)
	}
	if got, ok := envValue(gotEnvironment, "SSL_CERT_FILE"); !ok || got != "/operator/corporate-ca.pem" {
		t.Fatalf("SSL_CERT_FILE = %q (present %v): the forwarded bundle must pass verbatim", got, ok)
	}
	if got, ok := envValue(gotEnvironment, "SSL_CERT_DIR"); !ok || got != "" {
		t.Fatalf("SSL_CERT_DIR = %q (present %v): beside a forwarded file the dir stays defined-empty — resolving it would widen the operator's trust", got, ok)
	}
}

// The mirror of the above (#7889, Codex 4722401904): a forwarded
// SSL_CERT_DIR claims the hashed-dir slot for the operator's restricted CA
// set — a resolved SSL_CERT_FILE beside it would add the image's bundle
// roots the operator did not forward. The file pin keeps defined-empty.
func TestExecInvocationKeepsFilePinWhenCertDirForwarded(t *testing.T) {
	stubTrustPathStat(t, "/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/certs/")
	t.Setenv("SSL_CERT_DIR", "/operator/restricted.d")
	pathDir := t.TempDir()
	writeExecutableFixture(t, pathDir, "codex")
	t.Setenv("PATH", pathDir)
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	// SSL_CERT_DIR rides extras (the operator's claim); SSL_CERT_FILE is a pin.
	if err := execInvocation([]string{"codex", "1", "SSL_CERT_DIR", "1", "SSL_CERT_FILE", "codex"}); !errors.Is(err, wantErr) {
		t.Fatalf("execInvocation() error = %v, want test sentinel", err)
	}
	if got, ok := envValue(gotEnvironment, "SSL_CERT_DIR"); !ok || got != "/operator/restricted.d" {
		t.Fatalf("SSL_CERT_DIR = %q (present %v): the forwarded dir must pass verbatim", got, ok)
	}
	if got, ok := envValue(gotEnvironment, "SSL_CERT_FILE"); !ok || got != "" {
		t.Fatalf("SSL_CERT_FILE = %q (present %v): beside a forwarded dir the file stays defined-empty — resolving it would widen the operator's trust", got, ok)
	}
}

// #7889 R1: the agent-server is the parent of every non-pane child af
// starts in the container (post-worktree hooks, helpers). At startup it
// neutralizes the defined-empty trust pins in its OWN environment: a
// pinned name resolves to a real bundle, drops to unset on a store-less
// image, and a forwarded (non-empty) value is never touched — so no child
// ever inherits `NAME=`.
func TestNeutralizeTrustPathPinsFixesTheProcessEnv(t *testing.T) {
	stubTrustPathStat(t, "/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/certs/")
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	NeutralizeTrustPathPins()
	if got := os.Getenv("SSL_CERT_FILE"); got != "/etc/ssl/certs/ca-certificates.crt" {
		t.Fatalf("SSL_CERT_FILE = %q: a defined-empty pin must resolve to the platform bundle", got)
	}
	if got := os.Getenv("SSL_CERT_DIR"); got != "/etc/ssl/certs" {
		t.Fatalf("SSL_CERT_DIR = %q: a defined-empty pin must resolve to the hashed dir", got)
	}

	// A forwarded (non-empty) value is the operator's claim — untouched.
	t.Setenv("SSL_CERT_FILE", "/operator/corporate-ca.pem")
	NeutralizeTrustPathPins()
	if got := os.Getenv("SSL_CERT_FILE"); got != "/operator/corporate-ca.pem" {
		t.Fatalf("SSL_CERT_FILE = %q: a forwarded value must not be re-pointed", got)
	}
	// …and beside that forwarded file the dir pin keeps DEFINED-EMPTY
	// rather than resolving or unsetting: resolving would hand rustls the
	// image's hashed roots where the operator's file was meant to stand
	// alone, and unset would restore the platform default dir for OpenSSL
	// and openssl-probe alike — the empty slot must contribute NOTHING (R5).
	t.Setenv("SSL_CERT_DIR", "")
	NeutralizeTrustPathPins()
	if got, ok := os.LookupEnv("SSL_CERT_DIR"); !ok || got != "" {
		t.Fatalf("SSL_CERT_DIR must stay defined-empty beside a forwarded SSL_CERT_FILE, got %q (present %v)", os.Getenv("SSL_CERT_DIR"), ok)
	}

	// The mirror: beside a forwarded SSL_CERT_DIR the file pin keeps
	// defined-empty too — a resolved bundle would add the image's roots to
	// a trust the operator bounded to the forwarded dir (#7889, Codex
	// 4722401904).
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "/operator/certs.d")
	NeutralizeTrustPathPins()
	if got, ok := os.LookupEnv("SSL_CERT_FILE"); !ok || got != "" {
		t.Fatalf("SSL_CERT_FILE must stay defined-empty beside a forwarded SSL_CERT_DIR, got %q (present %v)", os.Getenv("SSL_CERT_FILE"), ok)
	}

	// On a store-less image the pin drops to unset — these children have
	// no .env-fill channel, so unset is the inert form.
	stubTrustPathStat(t)
	os.Unsetenv("SSL_CERT_FILE")
	t.Setenv("SSL_CERT_DIR", "")
	NeutralizeTrustPathPins()
	if _, ok := os.LookupEnv("SSL_CERT_DIR"); ok {
		t.Fatalf("SSL_CERT_DIR on a store-less image must drop to unset, got %q", os.Getenv("SSL_CERT_DIR"))
	}
}

// #7891 R3: the git strip names are never resolved — neither as pins (they
// are absent from trustPathPinCandidates) nor by the startup
// neutralization, which only re-points the SSL pair. The
// filter-at-the-shim boundary is what keeps them off the agent-server's
// environment unforwarded; the provisioning preamble unsets them. This
// test pins the "never resolved" half: a strip name in the process env —
// reachable at the HOST boundary where no filter ran (an unclassified
// local session, a test, a future caller) — survives neutralization
// byte-for-byte instead of being rewritten to an af-chosen platform path
// that would then outrank a config-route CA or widen a dir-only forward.
func TestNeutralizeTrustPathPinsLeavesStripNamesAlone(t *testing.T) {
	stubTrustPathStat(t, "/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/certs/")
	for _, name := range TrustStripNames() {
		t.Setenv(name, "image-supplied-value")
	}
	NeutralizeTrustPathPins()
	for _, name := range TrustStripNames() {
		if got := os.Getenv(name); got != "image-supplied-value" {
			t.Fatalf("%s = %q: a strip name must never resolve — unset at filtered hops is its only safe spelling (#7891 R3)", name, got)
		}
	}
}

// #7891/R8: a git TLS name in the exec environment is the image's claim —
// git reads GIT_SSL_NO_VERIFY by getenv, so any defined value disables
// verification (measured on git 2.43) and the only inert spelling is
// absent. There is no dedicated strip: FilterForCommand is an allowlist,
// so a name that is neither a pin, a program name, nor an extras-carried
// claim never reaches the exec'd child — this row proves the boundary
// holds for every strip-set name. A forwarded name rides extras verbatim.
func TestExecInvocationKeepsStripNamesOffUnforwarded(t *testing.T) {
	pathDir := t.TempDir()
	writeExecutableFixture(t, pathDir, "codex")
	t.Setenv("PATH", pathDir)
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	for _, name := range TrustStripNames() {
		t.Setenv(name, "attacker-or-image-value")
	}
	if err := execInvocation([]string{"codex", "0", "0", "codex"}); !errors.Is(err, wantErr) {
		t.Fatalf("execInvocation() error = %v, want test sentinel", err)
	}
	for _, name := range TrustStripNames() {
		if _, ok := envValue(gotEnvironment, name); ok {
			t.Fatalf("%s reached the agent's environment unforwarded — the allowlist must keep it off", name)
		}
	}

	// Forwarded through extras, the operator's value passes verbatim.
	gotEnvironment = nil
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	if err := execInvocation([]string{"codex", "1", "GIT_SSL_NO_VERIFY", "0", "codex"}); !errors.Is(err, wantErr) {
		t.Fatalf("execInvocation() error = %v, want test sentinel", err)
	}
	if got, ok := envValue(gotEnvironment, "GIT_SSL_NO_VERIFY"); !ok || got != "1" {
		t.Fatalf("GIT_SSL_NO_VERIFY = %q (present %v): a forwarded claim must reach the agent verbatim", got, ok)
	}
}

// With the operator forwarding the name it is not a pin at all — it rides
// extras — and the operator's CA value must reach the agent verbatim, not
// resolved over and not blanked.
func TestExecInvocationPassesForwardedTrustNameVerbatim(t *testing.T) {
	stubTrustPathStat(t, "/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/certs/")
	t.Setenv("SSL_CERT_FILE", "/operator/corporate-ca.pem")
	pathDir := t.TempDir()
	writeExecutableFixture(t, pathDir, "codex")
	t.Setenv("PATH", pathDir)
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	if err := execInvocation([]string{"codex", "1", "SSL_CERT_FILE", "0", "codex"}); !errors.Is(err, wantErr) {
		t.Fatalf("execInvocation() error = %v, want test sentinel", err)
	}
	if got, ok := envValue(gotEnvironment, "SSL_CERT_FILE"); !ok || got != "/operator/corporate-ca.pem" {
		t.Fatalf("SSL_CERT_FILE = %q (present %v): a forwarded trust name reaches the agent verbatim", got, ok)
	}
}
