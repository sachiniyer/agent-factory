package daemon

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// The VS Code editor's PER-SESSION preview origin (#2743).
//
// Terminal history — and every other piece of workbench state — crosses between
// sessions' editors, and the vector is the browser ORIGIN, not the shared user-data
// directory the issue was first filed against. code-server is VS Code *Web*, which
// keeps workbench state in the browser's IndexedDB; `vscode-web-state-db-global`
// holds `terminal.history.entries.commands` and `.dirs` and is NOT workspace-scoped.
// Because every session's editor is framed on the SPA's one origin, they all share
// that database, so one session's commands and checkout paths show up in another
// session's "Run Recent Command". On this box those command lines carry branch names,
// paths and occasionally tokens, and it looks exactly like the user's own history —
// so nobody would ever report it.
//
// The fix is to stop sharing the origin. That is #1856's machinery, with ONE
// difference that makes it a separate derivation rather than a flag on the existing
// one: an editor's origin must be STABLE.
//
// WHY A SECOND SECRET. A web tab's origin is HMAC(Manager.previewSecret, sid, tid),
// and previewSecret is in-memory and rotates on every daemon restart — right for a
// dev-server preview, which holds nothing worth keeping. An editor holds a great
// deal: layout, opened editors, and the very terminal history this issue is about,
// all in origin-scoped IndexedDB. A rotating origin would silently blank all of it on
// every daemon restart, which is not a fix but a second, quieter data loss. So the
// editor's origin derives from a secret PERSISTED at 0600 in the af home — the same
// posture the daemon bearer token already has — and the preview secret keeps its
// ephemeral lifetime, unchanged. Two secrets because they protect two things with
// genuinely different lifetimes; collapsing them would mean either weakening the
// preview posture or destroying editor state.
//
// WHY PER SESSION, NOT PER TAB. There is one code-server per session
// (ensureVSCodeServer keys on daemonInstanceKey(repoID, title)), so a per-TAB origin
// would put one editor behind two origins with two IndexedDBs — the same process
// disagreeing with itself about its own state. Per-session is also exactly the
// boundary the issue asks for: history scoped per session, with the user-data
// directory still shared so settings and extensions keep carrying across, which is
// why it is shared in the first place.
//
// WHAT THIS DOES NOT COVER, stated rather than discovered: *.localhost resolves to
// the BROWSER's own loopback, so a remote viewer keeps the shared-origin editor and
// keeps this leak. Closing it there needs an origin scheme that survives a single
// forwarded port, which no *.localhost scheme does.

// editorOriginSecretFileName is the persisted HMAC key behind every editor origin.
// 0600 in the af home, beside daemon-token, and for the same reason: it is bearer
// material for a surface that must outlive a restart.
const editorOriginSecretFileName = "editor-origin-secret" //nolint:gosec // file name, not a credential

// editorOriginSecretPath resolves the editor-origin secret's path in the af home.
func editorOriginSecretPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, editorOriginSecretFileName), nil
}

// ensureEditorOriginSecret loads the persisted editor-origin secret, minting it on
// first use. It mirrors EnsureToken exactly — 0700 directory, file lock, atomic 0600
// write — because it is the same kind of material with the same durability
// requirement, and a second hand-rolled implementation is how the two drift.
//
// Persistence IS the feature here: the value's whole job is to make an editor's
// origin the same name tomorrow as it was today, so the browser hands that editor
// back the IndexedDB it left behind.
func ensureEditorOriginSecret() (string, error) {
	path, err := editorOriginSecretPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create editor-origin secret directory: %w", err)
	}
	var resolved string
	err = config.WithFileLock(path, func() error {
		if secret, lerr := loadEditorOriginSecret(path); lerr == nil {
			resolved = secret
			return nil
		} else if !os.IsNotExist(lerr) {
			return lerr
		}
		secret, gerr := generateToken()
		if gerr != nil {
			return gerr
		}
		if werr := config.AtomicWriteFile(path, []byte(secret+"\n"), 0o600); werr != nil {
			return fmt.Errorf("write editor-origin secret: %w", werr)
		}
		resolved = secret
		return nil
	})
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// loadEditorOriginSecret reads the secret, treating an empty file as absent so a
// truncated write is re-minted rather than producing a derivation everyone shares.
func loadEditorOriginSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", os.ErrNotExist
	}
	return secret, nil
}

// editorOriginLabel derives the DNS label of one SESSION's editor origin. Same shape
// and alphabet as previewTabHostLabel (so previewHostLabel parses both and one gate
// covers them), but keyed on the session alone and under the PERSISTED secret — the
// two properties that make an editor's browser state survive a daemon restart.
//
// The "vscode" domain separator keeps this derivation disjoint from the web-tab one
// even if the two secrets were ever identical, so no session's editor origin can
// collide with any tab's preview origin.
func editorOriginLabel(secret, sessionID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("vscode"))
	mac.Write([]byte{0})
	mac.Write([]byte(sessionID))
	return previewLabelPrefix + strings.ToLower(previewLabelEncoding.EncodeToString(mac.Sum(nil)[:previewLabelBytes]))
}
