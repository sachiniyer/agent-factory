package daemon

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// The bearer-token material for the daemon's TCP/TLS surface (#1592 Phase 3
// PR1, §1.3). Sachin locked the auth model to a single bearer token = full
// access, single-owner. This file only produces and compares the material —
// it is DARK: nothing here binds a socket or enforces a token. Phase 3 PR2
// fills in the withAuth compare against this token; PR3 lights up the TCP
// listener. The unix control socket stays unauthenticated (filesystem 0600
// perms are the local auth, #1029) and never reads this token.

const (
	// daemonTokenFileName is the bearer token file in the af home. 0600 —
	// leaking it grants full daemon access under the locked auth model.
	daemonTokenFileName = "daemon-token"
	// tokenRandomBytes is the entropy drawn from crypto/rand per token: 256
	// bits, base64url-encoded to a 43-char printable string.
	tokenRandomBytes = 32
)

// TokenPath returns <af home>/daemon-token, the file the bearer token is
// persisted in. It mirrors DaemonSocketPath: the af home is resolved through
// config.GetConfigDir so AGENT_FACTORY_HOME is honored uniformly.
func TokenPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, daemonTokenFileName), nil
}

// generateToken returns a fresh bearer token: 256 bits from crypto/rand,
// base64url-encoded without padding (URL/header-safe, no '=' to escape).
func generateToken() (string, error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// writeToken persists tok to path with 0600 permissions, creating the parent
// af home directory if needed. It writes then chmods so the mode is exactly
// 0600 regardless of the process umask.
func writeToken(path, tok string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return fmt.Errorf("write token: %w", err)
	}
	// WriteFile only applies the mode on create and masks it by the umask;
	// force 0600 so a pre-existing file with looser perms is tightened too.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod token: %w", err)
	}
	return nil
}

// LoadToken reads and returns the persisted bearer token at path. It returns
// an error when the file is absent or unreadable — callers that must fail
// closed (the auth gate) treat any error as "deny". The stored value is
// trimmed of the trailing newline writeToken adds.
func LoadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

// EnsureToken returns the bearer token at path, generating and persisting one
// (0600) if the file does not yet exist. It is idempotent: a second call
// returns the same token. This is the generate-if-absent entry point behind
// `af token show`, so the token exists before the TCP listener is ever
// enabled.
func EnsureToken(path string) (string, error) {
	if tok, err := LoadToken(path); err == nil {
		return tok, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := writeToken(path, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// RotateToken generates a fresh bearer token, persists it (0600) over any
// existing one, and returns it. Because the auth gate re-reads the token file
// per auth event (§1.3), rotation takes effect for new connections
// immediately without a daemon RPC; existing streams keep running until they
// reconnect.
func RotateToken(path string) (string, error) {
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := writeToken(path, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// ConstantTimeEqual reports whether got equals want without leaking, through
// timing, how many leading bytes matched. It is the compare the withAuth gate
// uses in PR2. An empty want denies (fail closed): a daemon with no token must
// never accept the empty string as a valid credential.
func ConstantTimeEqual(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
