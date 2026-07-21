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

// The bearer-token material for the daemon's HTTP TCP surface (#1592 Phase 3,
// §1.3). The auth model is a single bearer token = full access, single-owner.
// This file produces and compares the material; the withAuth gate enforces it
// on the HTTP TCP listener (startTCPListener), reading it fresh per auth event so
// `af token rotate` takes effect without a restart. The unix control socket
// stays unauthenticated (filesystem 0600 perms are the local auth, #1029) and
// never reads this token.
//
// Two concurrency guarantees mirror the token-material fix (#1690):
//
//   - Generation is serialized under an exclusive file lock (config.WithFileLock,
//     the sibling daemon-token.lock). Before this, EnsureToken's LoadToken-miss →
//     generateToken → writeToken sequence was a TOCTOU race (#1720): two callers
//     (e.g. daemon startup and `af token show`) could both observe "no token" and
//     both generate, racing to persist different tokens. The lock makes the
//     first caller win and later callers observe its token, so every caller
//     agrees.
//   - The write is atomic (config.AtomicWriteFile: temp file + rename). Before
//     this, writeToken used os.WriteFile (O_TRUNC), so a rotation left a window
//     where the file was truncated-but-not-yet-rewritten. The withAuth gate
//     re-reads the token per request (authorize → expectedToken → LoadToken), so
//     a request landing in that window saw an empty file, LoadToken errored, and
//     the gate failed closed => a spurious 401 (#1722). An atomic rename lets a
//     reader observe only the whole old or whole new token, never a partial.
//
// Readers (the auth gate) deliberately do NOT take the lock: they do a plain
// LoadToken so rotation stays live per request, and the atomic write is what
// keeps that lock-free read from ever seeing a torn value.

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

// writeToken persists tok to path atomically with 0600 permissions, creating
// the parent af home directory if needed. It writes to a temp file and renames
// it into place (config.AtomicWriteFile), so a concurrent reader never observes
// a truncated/empty token file mid-write (the #1722 rotation-401 window).
// AtomicWriteFile applies perm exactly via tmp.Chmod, so the token lands 0600
// regardless of the process umask, and it MkdirAll's the parent dir itself —
// no separate MkdirAll/Chmod needed.
func writeToken(path, tok string) error {
	if err := config.AtomicWriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return fmt.Errorf("write token: %w", err)
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
// (0600) if the file does not yet exist. It is idempotent even under concurrent
// callers: the check-then-generate-then-write runs under an exclusive file lock
// (config.WithFileLock), so two callers racing on a missing token (e.g. daemon
// startup and `af token show`) can no longer both generate — the first inside
// the lock writes it, the rest observe it and return the same value (#1720).
// This is the generate-if-absent entry point behind `af token show`, so the
// token exists before the TCP listener is ever enabled.
func EnsureToken(path string) (string, error) {
	// Pre-create the token directory 0700 even when a caller supplies a path
	// outside AGENT_FACTORY_HOME. WithFileLock also secures the configured AF
	// home, but bearer-token material must not rely on that path relationship.
	// A legacy default home is tightened by WithFileLock after this MkdirAll,
	// which cannot change an existing directory's mode on its own (#2197).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create token directory: %w", err)
	}
	var resolved string
	err := config.WithFileLock(path, func() error {
		if tok, err := LoadToken(path); err == nil {
			resolved = tok
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		tok, err := generateToken()
		if err != nil {
			return err
		}
		if err := writeToken(path, tok); err != nil {
			return err
		}
		resolved = tok
		return nil
	})
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// RotateToken generates a fresh bearer token, persists it (0600) over any
// existing one, and returns it. The generate+write runs under the same
// exclusive file lock as EnsureToken (config.WithFileLock) so a rotation
// serializes against a concurrent first-time generation — the two can't
// interleave and clobber each other (mirrors the cert template, #1690).
// Because the auth gate re-reads the token file per auth event (§1.3) and the
// write is atomic, rotation takes effect for new connections immediately
// without a daemon RPC and no reader ever sees a partial token; existing
// streams keep running until they reconnect.
func RotateToken(path string) (string, error) {
	// Preserve EnsureToken's owner-only guarantee for arbitrary token paths;
	// WithFileLock handles legacy permission repair inside AGENT_FACTORY_HOME.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create token directory: %w", err)
	}
	var resolved string
	err := config.WithFileLock(path, func() error {
		tok, err := generateToken()
		if err != nil {
			return err
		}
		if err := writeToken(path, tok); err != nil {
			return err
		}
		resolved = tok
		return nil
	})
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// ConstantTimeEqual reports whether got equals want without leaking, through
// timing, how many leading bytes matched. It is the compare the withAuth gate
// uses. An empty want denies (fail closed): a daemon with no token must
// never accept the empty string as a valid credential.
func ConstantTimeEqual(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
