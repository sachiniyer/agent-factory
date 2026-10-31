package runtimetoken

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useTempAFHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	return dir
}

// The key is minted once, at 0600, and every later call returns the SAME bytes —
// it is the root every live session's token derives from, so a silent remint
// would cut the credential of every still-running sandbox.
func TestEnsureKeyMintsAPersistent0600Key(t *testing.T) {
	home := useTempAFHome(t)

	key, err := EnsureKey()
	if err != nil {
		t.Fatalf("EnsureKey() error = %v", err)
	}
	if len(key) != keyRawBytes {
		t.Fatalf("key length = %d, want %d", len(key), keyRawBytes)
	}

	info, err := os.Stat(filepath.Join(home, KeyFileName))
	if err != nil {
		t.Fatalf("key file missing: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("key file mode = %o, want 0600 — it is the derivation root for every live sandbox token", got)
	}
	// The file encodes the raw key; anything else would make LoadKey disagree
	// with what EnsureKey returned.
	data, err := os.ReadFile(filepath.Join(home, KeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || string(decoded) != string(key) {
		t.Fatal("key file does not decode back to the minted key")
	}

	again, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(key) {
		t.Fatal("EnsureKey reminted an existing key — that silently rotates every live session's credential")
	}
	loaded, err := LoadKey()
	if err != nil || string(loaded) != string(key) {
		t.Fatalf("LoadKey() = (%v, %v), want the minted key", loaded, err)
	}
}

// A corrupt key file must fail loudly. Reminting over it is the silent rotation
// this function exists to prevent, so the error — not a new key — is the right
// answer.
func TestEnsureKeyRefusesACorruptKey(t *testing.T) {
	home := useTempAFHome(t)
	path := filepath.Join(home, KeyFileName)
	if err := os.WriteFile(path, []byte("not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(); err == nil {
		t.Fatal("LoadKey accepted a corrupt key file")
	}
	if _, err := EnsureKey(); err == nil {
		t.Fatal("EnsureKey reminted over a corrupt key — a silent rotation of every live credential")
	}
	if data, _ := os.ReadFile(path); string(data) != "not-a-key\n" {
		t.Fatal("the corrupt file must be left alone for an operator to inspect, not overwritten")
	}
}

// The reference is what the row persists: scheme-prefixed so a future
// derivation can coexist, unique per mint, and carrying only a nonce.
func TestNewTokenRefIsSchemedUniqueAndNonceShaped(t *testing.T) {
	a, err := NewTokenRef()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTokenRef()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two mints produced the same reference")
	}
	nonce, ok := strings.CutPrefix(a, refSchemeV1+".")
	if !ok {
		t.Fatalf("ref %q lacks the %q scheme prefix", a, refSchemeV1+".")
	}
	raw, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(raw) != refNonceBytes {
		t.Fatalf("ref nonce does not decode to %d bytes: %v", refNonceBytes, err)
	}
}

// Derivation is deterministic — that IS the contract: ref + key reproduces the
// token the remote expects — and a ref naming a scheme this binary does not
// know is refused rather than improvised under.
func TestDeriveAgentServerToken(t *testing.T) {
	key := make([]byte, keyRawBytes)
	for i := range key {
		key[i] = byte(i)
	}
	ref, err := NewTokenRef()
	if err != nil {
		t.Fatal(err)
	}

	token, err := DeriveAgentServerToken(key, ref)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DeriveAgentServerToken(key, ref)
	if err != nil || again != token {
		t.Fatalf("derivation is not deterministic: (%q, %v) vs %q", again, err, token)
	}
	if len(token) != 43 || strings.ContainsRune(token, '=') {
		t.Fatalf("token %q is not an unpadded base64url SHA-256", token)
	}
	otherRef, _ := NewTokenRef()
	if other, err := DeriveAgentServerToken(key, otherRef); err != nil || other == token {
		t.Fatal("a different ref derived the same token")
	}
	otherKey := make([]byte, keyRawBytes)
	if other, err := DeriveAgentServerToken(otherKey, ref); err != nil || other == token {
		t.Fatal("a different key derived the same token")
	}

	for _, bad := range []string{"", "v1.", "v2.nonce", "nonsense", ".abc"} {
		if _, err := DeriveAgentServerToken(key, bad); err == nil {
			t.Fatalf("ref %q derived a token instead of failing closed", bad)
		}
	}
}

// The remote side reads the token from beside its own binary — the design that
// keeps the token's path out of argv and environment. A missing file is a
// pre-W1.1 server and answers ""; a file that exists but is unusable is a fact
// about the sandbox worth refusing, not a reason to fall back to the ambient
// daemon-token.
func TestSuppliedAgentServerToken(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "af")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := executable
	executable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executable = previous })
	tokenPath := filepath.Join(dir, TokenFileName)

	// Absent: the pre-W1.1 shape, no error.
	if got, err := SuppliedAgentServerToken(); err != nil || got != "" {
		t.Fatalf("missing file = (%q, %v), want an empty token and no error", got, err)
	}

	// A regular file, with the trailing newline provisioning writes, yields the token.
	if err := os.WriteFile(tokenPath, []byte("tok-abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := SuppliedAgentServerToken(); err != nil || got != "tok-abc" {
		t.Fatalf("readable token file = (%q, %v), want tok-abc", got, err)
	}

	// An empty file is an error, not a fallback.
	if err := os.WriteFile(tokenPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SuppliedAgentServerToken(); err == nil {
		t.Fatal("an empty token file must refuse, not fall back to a shared credential")
	}

	// A symlink is refused — Lstat, not Stat, so the link itself is judged.
	linkTarget := filepath.Join(t.TempDir(), "real-token")
	if err := os.WriteFile(linkTarget, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tokenPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkTarget, tokenPath); err != nil {
		t.Fatal(err)
	}
	if _, err := SuppliedAgentServerToken(); err == nil {
		t.Fatal("a symlinked token file must be refused, not followed")
	}

	// A directory at the path is not a regular file either.
	if err := os.Remove(tokenPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tokenPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := SuppliedAgentServerToken(); err == nil {
		t.Fatal("a directory at the token path must be refused")
	}
}

// The callback credential derives from (key, session, ref) so a restarted
// daemon can re-register the secret a sandbox already holds (#5571 W3.6).
func TestDeriveCallbackToken(t *testing.T) {
	key := make([]byte, keyRawBytes)
	for i := range key {
		key[i] = byte(i)
	}
	ref, err := NewTokenRef()
	if err != nil {
		t.Fatal(err)
	}
	derive := func(k []byte, session, r string) string {
		t.Helper()
		tok, err := DeriveCallbackToken(k, session, r)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	a := derive(key, "sess-a", ref)
	if again := derive(key, "sess-a", ref); again != a {
		t.Fatal("deriving twice from the same inputs must give the same secret; restart durability rests on it")
	}
	if derive(key, "sess-b", ref) == a {
		t.Fatal("one ref must never yield the same credential for two sessions")
	}
	otherRef, _ := NewTokenRef()
	if derive(key, "sess-a", otherRef) == a {
		t.Fatal("a new ref must be a new credential")
	}
	if server, _ := DeriveAgentServerToken(key, ref); server == a {
		t.Fatal("the callback and agent-server credentials must never be interchangeable")
	}
	otherKey := append([]byte(nil), key...)
	otherKey[0] ^= 0xff
	if derive(otherKey, "sess-a", ref) == a {
		t.Fatal("a different key must derive a different credential")
	}
	if _, err := DeriveCallbackToken(key, "sess-a", "v9.abc"); err == nil {
		t.Fatal("an unknown scheme must fail closed")
	}
	if _, err := DeriveCallbackToken(key, " ", ref); err == nil {
		t.Fatal("no session id must mean no credential")
	}
}
