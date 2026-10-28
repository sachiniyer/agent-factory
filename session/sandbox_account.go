package session

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Session-scoped account sync for listed hosts (#5571 W7.3).
//
// The session's selected account is copied into a per-session directory INSIDE
// the session dir on the host — <sessionDir>/account — and the agent is pointed
// at it with the agent's own account variable. It never touches the remote
// user's global agent config, so the host's own login is neither read nor
// replaced; and it is removed with the session.
//
// WHO CHOOSES THE DESTINATION is the confused-deputy line, and it is drawn
// here: the only licence to send credentials is ProvisionSpec.AccountSyncHost,
// which the ssh runtime sets only when the host resolved to an entry in the
// operator's global host list. A target a repository named (its [ssh] section,
// a sandbox command) carries no licence, so an account on it is refused before
// anything is sent.
//
// Only the account's credential FILES travel — the same minimal surface
// agentCredentialFiles documents for docker — as a tar built here and written to
// the shell's STDIN. A secret never appears in argv, a script, or a log line.
//
// Sessions on one host share its ssh user, so they can read each other's
// workspaces and synced credentials, exactly as local worktrees share one user
// today (settled decision 3). That is documented, not a gap this slice closes.

// accountCredentialRel lists, per agent, the credential files inside an account
// directory (an AGENT HOME, see sessionenv.Account.Dir) that a sync carries.
// Everything else in the account dir — history, settings, skills — stays on the
// daemon box. The relative names are also the ONLY names a write-back accepts.
var accountCredentialRel = map[string][]string{
	tmux.ProgramClaude: {".credentials.json"},
	tmux.ProgramCodex:  {"auth.json"},
	tmux.ProgramGemini: {
		".gemini/oauth_creds.json",
		".gemini/gemini-credentials.json",
		".gemini/google_accounts.json",
	},
}

const (
	sshAccountSubdir = "account"
	// accountSyncMaxFileBytes bounds one credential file in either direction.
	// Real ones are a few KiB; the bound keeps a hostile host's archive from
	// filling the daemon's memory during write-back.
	accountSyncMaxFileBytes = 1 << 20
)

// AccountDir is the session-scoped account directory on the host.
func (w *sandboxWorkspace) AccountDir() string { return w.SessionDir + "/" + sshAccountSubdir }

// accountEnv is the variable that points the agent at the scoped account dir,
// from the same table the local launch uses. ok is false for an agent with no
// relocatable credential root.
func (w *sandboxWorkspace) accountEnv() (name, value string, ok bool) {
	if w.spec.Account.Dir == "" {
		return "", "", false
	}
	name, ok = sessionenv.SupportsAccounts(w.spec.Account.Agent)
	if !ok {
		return "", "", false
	}
	return name, w.AccountDir(), true
}

// accountSyncApplies reports whether this provision carries an account, and
// refuses — before anything is sent — when it has no licence to.
func (w *sandboxWorkspace) accountSyncApplies() (bool, error) {
	if w.spec.Account.Dir == "" {
		return false, nil
	}
	if strings.TrimSpace(w.spec.AccountSyncHost) == "" {
		return false, fmt.Errorf("account %q is not synced: the target is not in the global host list, and credentials are only ever sent to a listed host; add the host under [hosts.<name>] or omit --account",
			w.spec.Account.Name)
	}
	if _, ok := accountCredentialRel[w.spec.Account.Agent]; !ok {
		return false, fmt.Errorf("account %q cannot be synced: agent %q has no credential files af knows how to carry",
			w.spec.Account.Name, w.spec.Account.Agent)
	}
	return true, nil
}

// syncAccountIn copies the account's credential files into the session-scoped
// directory. A no-op without an account; an error without a licence.
func (w *sandboxWorkspace) syncAccountIn(timeout time.Duration) error {
	apply, err := w.accountSyncApplies()
	if err != nil || !apply {
		return err
	}
	archive, n, err := buildAccountArchive(w.spec.Account.Dir, accountCredentialRel[w.spec.Account.Agent])
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("account %q has no credential file to sync under %s (has it been logged in?)", w.spec.Account.Name, w.spec.Account.Dir)
	}
	dir := shellQuote(w.AccountDir())
	script := "umask 077 && mkdir -p " + dir + " && tar -xf - -C " + dir
	if out, err := w.shell.Run(timeout, script, bytes.NewReader(archive), true); err != nil {
		return fmt.Errorf("syncing account %q to the host failed: %s: %w", w.spec.Account.Name, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// pullAccountBack reads the credential files back from the host and replaces
// the registry copy when one changed — a refresh the agent wrote there. Only the
// allowlisted names are accepted, whatever the host's archive contains.
func (w *sandboxWorkspace) pullAccountBack(timeout time.Duration) (changed int, err error) {
	apply, err := w.accountSyncApplies()
	if err != nil || !apply {
		return 0, err
	}
	rels := accountCredentialRel[w.spec.Account.Agent]
	quoted := make([]string, 0, len(rels))
	for _, rel := range rels {
		quoted = append(quoted, shellQuote(rel))
	}
	// Missing files are skipped, and a scope that is already gone (a repeated
	// teardown) reads back as nothing rather than as an error.
	script := "cd " + shellQuote(w.AccountDir()) + " 2>/dev/null || exit 0; " +
		"list=$(for f in " + strings.Join(quoted, " ") + "; do [ -f \"$f\" ] && printf '%s\\n' \"$f\"; done); " +
		"[ -n \"$list\" ] || exit 0; printf '%s\\n' \"$list\" | tar -cf - -T -"
	out, runErr := w.shell.Run(timeout, script, nil, false)
	if runErr != nil {
		return 0, fmt.Errorf("reading account %q back from the host failed: %w", w.spec.Account.Name, runErr)
	}
	if len(out) == 0 {
		return 0, nil
	}
	files, err := readAccountArchive(out, rels)
	if err != nil {
		return 0, fmt.Errorf("account %q write-back refused: %w", w.spec.Account.Name, err)
	}
	for rel, data := range files {
		path := filepath.Join(w.spec.Account.Dir, filepath.FromSlash(rel))
		if cur, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(cur, data) {
			continue
		}
		if err := writeFileAtomic0600(path, data); err != nil {
			return changed, fmt.Errorf("writing refreshed account %q back failed: %w", w.spec.Account.Name, err)
		}
		changed++
	}
	return changed, nil
}

// removeAccount deletes the scoped account directory from the host.
func (w *sandboxWorkspace) removeAccount(timeout time.Duration) error {
	if w.spec.Account.Dir == "" || w.SessionDir == "" {
		return nil
	}
	if out, err := w.shell.Run(timeout, "rm -rf "+shellQuote(w.AccountDir()), nil, true); err != nil {
		return fmt.Errorf("removing the synced account from the host failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// teardownAccount is the account's part of a teardown: a last write-back, then
// removal. Write-back failure never blocks the removal — credentials left on a
// host are the worse outcome than a lost refresh — and is reported alongside it.
func (w *sandboxWorkspace) teardownAccount(timeout time.Duration) error {
	if w.spec.Account.Dir == "" {
		return nil
	}
	var pullErr error
	if strings.TrimSpace(w.spec.AccountSyncHost) != "" {
		_, pullErr = w.pullAccountBack(timeout)
	}
	rmErr := w.removeAccount(timeout)
	switch {
	case rmErr != nil && pullErr != nil:
		return fmt.Errorf("%w (and the final write-back failed: %v)", rmErr, pullErr)
	case rmErr != nil:
		return rmErr
	default:
		return pullErr
	}
}

func buildAccountArchive(dir string, rels []string) ([]byte, int, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	n := 0
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, 0, fmt.Errorf("reading account credential %s: %w", rel, err)
		}
		if len(data) > accountSyncMaxFileBytes {
			return nil, 0, fmt.Errorf("account credential %s is %d bytes, over the %d-byte sync bound", rel, len(data), accountSyncMaxFileBytes)
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, 0, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, 0, err
		}
		n++
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), n, nil
}

// readAccountArchive parses an archive from the HOST — untrusted input — and
// returns only entries whose name is exactly one of allowed, as regular files
// within the size bound. Anything else refuses the whole archive: a name that is
// not on the list, a link, a directory traversal.
func readAccountArchive(data []byte, allowed []string) (map[string][]byte, error) {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("unreadable archive: %w", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if !ok[name] || h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("unexpected archive entry %q", h.Name)
		}
		if h.Size > accountSyncMaxFileBytes {
			return nil, fmt.Errorf("archive entry %q is over the %d-byte bound", h.Name, accountSyncMaxFileBytes)
		}
		b, err := io.ReadAll(io.LimitReader(tr, accountSyncMaxFileBytes+1))
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
}

func writeFileAtomic0600(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".af-account-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
