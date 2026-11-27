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
// Defined-empty for SSL_CERT_FILE/SSL_CERT_DIR and the CA-bundle spellings is
// a PATH their TLS stacks load — OpenSSL and rustls-native-certs read the
// empty value and drop every platform root — so the exec shim re-asserts each
// name to a real bundle location instead. These tables run the resolution and
// the exec boundary that applies it; on master every row emitted NAME= and
// zeroed the container's trust store.

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
		// names the second entry wins; for SSL_CERT_DIR the dir spelling.
		var existing []string
		for _, c := range candidates[1:] {
			if name == "SSL_CERT_DIR" {
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
// selectors, and the empty-equals-unset names keep their defined-empty pin
// through the generic branch.
func TestResolveTrustPathPinLeavesNonPathNamesAlone(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "GH_HOST", "AF_DAEMON_TOKEN", "NOT_A_NAME"} {
		if _, ok := resolveTrustPathPin(name); ok {
			t.Fatalf("%s must not resolve as a path-valued pin", name)
		}
	}
}

// The table the session side consumes: the provisioning exec preamble unsets
// exactly these names, so the set must stay the audited path-valued class.
func TestTrustPathPinNamesListsTheAuditedClass(t *testing.T) {
	want := []string{
		"CODEX_CA_CERTIFICATE",
		"CURL_CA_BUNDLE",
		"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH",
		"NODE_EXTRA_CA_CERTS",
		"REQUESTS_CA_BUNDLE",
		"SSL_CERT_DIR",
		"SSL_CERT_FILE",
	}
	got := TrustPathPinNames()
	if !slices.Equal(got, want) {
		t.Fatalf("TrustPathPinNames() = %v, want the sorted audited set %v", got, want)
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
	err := execInvocation([]string{"codex", "0", "3", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "codex"})
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
	if slices.Contains(gotEnvironment, "SSL_CERT_FILE=attacker-or-image-value") {
		t.Fatal("a value re-set between docker run and the exec survived the trust-path pin")
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
