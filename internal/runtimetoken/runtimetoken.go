// Package runtimetoken owns the per-session bearer credential an off-box
// `af agent-server` requires (#5571 W1.1): how the token is derived, what the
// session row persists in its place, and how the remote process receives it.
//
// The token itself is never persisted. What a live ssh/sandbox row stores is
// an opaque REFERENCE — a nonce — and the credential is re-derived at need
// from a 0600 key file the daemon keeps under its AF home. That key is
// deliberately a SEPARATE file from daemon-token: `af token rotate` must not
// silently cut the credential a still-running sandbox's agent-server expects.
//
// On the remote the agent-server reads the token from a 0600 file written
// beside its own streamed binary and located through os.Executable — not a
// flag and not an environment variable — so neither the token nor the path
// can leak into anything the agent-server spawns. The only variable allowed
// into its environment is the nonce (RefEnvName), which is not a secret and
// exists so a host-side /proc/<pid>/environ probe can bind the live process
// to the persisted row; the probe extracts that name and nothing else.
package runtimetoken

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

const (
	// KeyFileName is the daemon-side 0600 key file under AF_HOME that every
	// per-session agent-server token derives from.
	KeyFileName = "sandbox-runtime-key"
	// TokenFileName is the remote-side 0600 file the provision step writes
	// beside the streamed af binary; `af agent-server` reads its expected
	// bearer token from it.
	TokenFileName = "af-server-token"
	// RefEnvName is the ONLY environment variable that carries the launch's
	// token reference into the agent-server's exec-time environment. It rides
	// the agent-server handoff's per-invocation extras — never sessionenv's
	// common allowlist — so it reaches THIS process without becoming an
	// inherited variable in the panes the server spawns. A host-side
	// /proc/<pid>/environ probe may extract exactly this name and nothing
	// else (#5571 W1.1 approval).
	RefEnvName = "AF_TOKEN_REF"

	// refSchemeV1 prefixes every minted reference so a future derivation
	// scheme can coexist in persisted rows instead of overloading the nonce.
	refSchemeV1 = "v1"
	// refNonceBytes is the entropy behind the reference: enough that a ref is
	// unguessable, but the nonce is only a lookup key — the 0600 key file is
	// what makes derivation unforgeable.
	refNonceBytes = 16
	// keyRawBytes is the entropy the key file encodes.
	keyRawBytes = 32
	// tokenMaxBytes bounds the file the remote side reads: a derived token is
	// 43 chars, and a hand-supplied one far larger than this is not ours.
	tokenMaxBytes = 256
)

// configDir and executable are test seams: the key must live in a temp AF home
// and the remote-side token file next to a staged binary, neither of which
// production paths arrange.
var (
	configDir  = config.GetConfigDir
	executable = os.Executable
)

// KeyPath returns <af home>/sandbox-runtime-key.
func KeyPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, KeyFileName), nil
}

// EnsureKey returns the runtime-identity key, generating and persisting it
// (0600) when the file does not yet exist. The discipline mirrors
// daemon.EnsureToken: an exclusive file lock around read-or-create so two
// provisions racing a missing key cannot mint different ones, and an atomic
// write that REFUSES a symlink so a planted link fails loudly rather than
// being followed into a write.
func EnsureKey() ([]byte, error) {
	path, err := KeyPath()
	if err != nil {
		return nil, err
	}
	// Bearer material must not rely on the caller having created the home —
	// same precondition daemon.EnsureToken takes before its file lock.
	if err := config.MkdirAllUnderAFHome(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create runtime-identity key directory: %w", err)
	}
	var key []byte
	err = config.WithFileLock(path, func() error {
		if k, err := LoadKey(); err == nil {
			key = k
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		raw := make([]byte, keyRawBytes)
		if _, err := rand.Read(raw); err != nil {
			return fmt.Errorf("generate runtime-identity key: %w", err)
		}
		if err := config.AtomicWriteFileRefusingLink(path,
			[]byte(base64.RawURLEncoding.EncodeToString(raw)+"\n"), 0o600); err != nil {
			return err
		}
		key = raw
		return nil
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

// LoadKey reads the existing runtime-identity key. A missing file surfaces as
// os.IsNotExist so EnsureKey can tell "not minted yet" from corruption, and so
// a reader can treat the absence as "no derived token exists".
func LoadKey() ([]byte, error) {
	path, err := KeyPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != keyRawBytes {
		return nil, fmt.Errorf("runtime-identity key file %s is unreadable as a key", path)
	}
	return key, nil
}

// NewTokenRef mints the opaque reference a session row persists instead of the
// token itself — "v1.<128-bit nonce>". The nonce rather than the session id is
// what derivation keys on: the spec a provisioner holds carries no session id,
// and a nonce survives archived-title reuse, where two sessions share a name.
// It is not a secret — knowing it without the key derives nothing.
func NewTokenRef() (string, error) {
	var b [refNonceBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint agent-server token reference: %w", err)
	}
	return refSchemeV1 + "." + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// DeriveAgentServerToken computes the bearer token the agent-server launched
// under ref expects: HMAC-SHA256(key, "af-agent-server/v1/"+nonce), base64url.
// A ref carrying any other scheme is refused rather than derived under the
// wrong construction — a persisted row naming a scheme this binary does not
// know must fail closed, not improvise.
func DeriveAgentServerToken(key []byte, ref string) (string, error) {
	nonce, ok := strings.CutPrefix(ref, refSchemeV1+".")
	if !ok || nonce == "" {
		return "", fmt.Errorf("agent-server token reference has an unknown scheme")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("af-agent-server/" + refSchemeV1 + "/" + nonce))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// DeriveCallbackToken computes the sandbox callback credential for one session
// under ref (#5571 W3.6): HMAC-SHA256(key, "af-sandbox-callback/v1/"+sessionID+
// "/"+nonce), base64url. Deriving it, rather than drawing a random secret that
// lives only in the daemon's memory, is what lets a restarted daemon accept the
// credential a still-running sandbox already holds: the row persists only the
// ref, the daemon re-derives the same secret from its 0600 key and re-registers
// it, and nothing has to be re-delivered to the sandbox. The session id is part
// of the input so one ref can never yield a credential for another session, and
// the label separates this construction from DeriveAgentServerToken's so the two
// credentials are never interchangeable.
func DeriveCallbackToken(key []byte, sessionID, ref string) (string, error) {
	nonce, ok := strings.CutPrefix(ref, refSchemeV1+".")
	if !ok || nonce == "" {
		return "", fmt.Errorf("sandbox callback reference has an unknown scheme")
	}
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("cannot derive a sandbox callback token without a session id")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("af-sandbox-callback/" + refSchemeV1 + "/" + sessionID + "/" + nonce))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// argv0 is a test seam for the spelling this process was launched under.
var argv0 = func() string {
	if len(os.Args) > 0 {
		return os.Args[0]
	}
	return ""
}

// sessionLaunchDir is the directory the session's own token sits in: the
// directory of the path THIS session launched the binary under. With one af
// binary shared per host (#5571 W2.2) the session's <sessionDir>/af is a
// symlink to the shared cache file, and os.Executable resolves through it — so
// the executable's own directory is the HOST-WIDE cache, which no session may
// look in. The launch path is used only when it is absolute and names the very
// file that is running; anything else falls back to the executable's directory,
// the pre-W2.2 behavior, and never to a lookup keyed to anything shared.
func sessionLaunchDir(exe, launch string) string {
	if filepath.IsAbs(launch) {
		if a, err := os.Stat(launch); err == nil {
			if b, err := os.Stat(exe); err == nil && os.SameFile(a, b) {
				return filepath.Dir(launch)
			}
		}
	}
	return filepath.Dir(exe)
}

// SessionDir is this agent-server's own session directory on the sandbox host —
// the directory beside the binary a provision staged, where the per-session
// token already lives. State that must survive an agent-server restart but belongs
// to this one session (and must not touch the host's shared af home) goes here.
func SessionDir() (string, error) {
	exe, err := executable()
	if err != nil {
		return "", fmt.Errorf("agent-server cannot resolve its own binary path: %w", err)
	}
	return sessionLaunchDir(exe, argv0()), nil
}

// SuppliedAgentServerToken reads the token a W1.1 provision placed beside THIS
// binary's session-specific launch path — <dirname(argv[0])>/af-server-token — when one exists. It returns
// "" when the file is absent: an agent-server provisioned before W1.1, or run
// by hand, keeps the file-backed daemon-token of the AF home it runs under.
//
// The lookup is a fixed name beside the executable on purpose: the provisioned
// binary lives at <sessionDir>/af, so no flag and no environment variable has
// to carry the path — which is what keeps the token's location out of argv and
// out of the environment everything this server spawns inherits.
//
// Fail-closed: a path that exists but is not a regular readable file is an
// error, not a silent fallback. Provisioning wrote that file itself, so a
// symlink, a directory or an empty file is a fact about the sandbox worth
// surfacing rather than authenticating under the wrong token.
func SuppliedAgentServerToken() (string, error) {
	exe, err := executable()
	if err != nil {
		return "", fmt.Errorf("agent-server cannot resolve its own binary path: %w", err)
	}
	path := filepath.Join(sessionLaunchDir(exe, argv0()), TokenFileName)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect agent-server token file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("agent-server token file %s is not a regular file; refusing it", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read agent-server token file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" || len(token) > tokenMaxBytes {
		return "", fmt.Errorf("agent-server token file %s holds an unusable token", path)
	}
	return token, nil
}
