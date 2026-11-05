package probe

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The credential gate. A Codex account logged in with ChatGPT holds OAuth
// tokens in <account-home>/auth.json; codex refreshes the access token when
// it is within roughly five minutes of expiry, and a refresh ROTATES the
// refresh token — the file on disk is rewritten with the pair. That rotation
// is the danger this boundary exists to prevent (#6787 review): af's sandbox
// flow copies auth.json into a session, and a copy the probe rotated is dead
// forever — the copy still holds the OLD refresh token, which the provider
// has already invalidated. A probe that refreshes is a second refresher
// racing every live copy of the same credential.
//
// The gate therefore asks one question — "can this home's refresh state be
// proven safe to read right now?" — and refuses when the answer is not yes:
//
//   - auth.lock present and held → a live copy is mid-refresh; refuse.
//     Newer Codex versions serialize refresh on this file. Held here means
//     flock: a copy that honors the same lock cannot start a refresh while
//     the probe holds it for its whole read window.
//   - API-key auth (OPENAI_API_KEY) → no refresh machinery exists; safe.
//   - tokens without a refresh token → nothing can be rotated; safe.
//   - an access-token exp far enough out that even a run hitting the whole
//     timeout cannot cross the refresh lead → safe.
//   - anything else — missing file, unparseable JSON, a non-JWT access
//     token, an exp inside the window, or metadata whose shape af cannot
//     read — is unproven, and unproven refuses.
const (
	codexAuthFile     = "auth.json"
	codexAuthLock     = "auth.lock"
	authFileCap       = 1 << 20
	codexRefreshLead  = 5 * time.Minute // codex's refresh-ahead-of-expiry window
	expiryCheckMargin = 30 * time.Second
)

var errSymlinkedAuthLock = errors.New("auth.lock is not a regular file")

// authProbeable reports whether the account home's credential state lets a
// probe run without ever becoming a second refresher. ok carries the release
// for the auth lock when one was held — callers run the exec inside that
// hold and release it when the run ends, so a copy honoring the same lock is
// serialized against the whole probe, not just the read.
func authProbeable(dir string, now time.Time, timeout time.Duration) (release func(), ok bool) {
	release = func() {}
	if _, err := os.Lstat(filepath.Join(dir, codexAuthLock)); err == nil {
		rel, err := holdAuthLock(filepath.Join(dir, codexAuthLock))
		if err != nil {
			return nil, false // held by a live copy, or unverifiable
		}
		release = rel
	} else if !os.IsNotExist(err) {
		return nil, false // the check itself could not run
	}

	data, err := readFileCapped(filepath.Join(dir, codexAuthFile), authFileCap)
	if err != nil {
		release()
		return nil, false
	}
	var auth struct {
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
		Tokens       struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		release()
		return nil, false
	}
	switch {
	case strings.TrimSpace(auth.OpenAIAPIKey) != "":
		return release, true // API-key auth: no refresh path exists
	case strings.TrimSpace(auth.Tokens.AccessToken) == "":
		release()
		return nil, false // nothing codex could authenticate with — metadata missing
	case auth.Tokens.RefreshToken == "":
		return release, true // nothing held that a refresh could rotate
	}
	exp, ok := jwtExpiry(auth.Tokens.AccessToken)
	if !ok {
		release()
		return nil, false
	}
	// Safe only when the access token cannot reach the refresh lead during
	// the run: exp must outlive now + lead + the whole timeout + skew. A
	// token inside that window could refresh mid-run — and rotating here
	// kills every sandbox copy holding the old pair.
	needAfter := now.Add(codexRefreshLead + timeout + expiryCheckMargin)
	if !exp.After(needAfter) {
		release()
		return nil, false
	}
	return release, true
}

// jwtExpiry reads the exp claim out of a JWT's payload — decode only, never
// verification: the signature is the provider's business; the probe needs
// the expiry number the CLI will use. Anything that is not a three-part JWT
// with a numeric exp is unproven.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

func readFileCapped(path string, cap int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, cap))
}
