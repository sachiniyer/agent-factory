package session

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
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
	archive, versions, err := buildAccountArchive(w.spec.Account.Dir, accountCredentialRel[w.spec.Account.Agent])
	if err != nil {
		return err
	}
	if len(versions) == 0 {
		return fmt.Errorf("account %q has no credential file to sync under %s (has it been logged in?)", w.spec.Account.Name, w.spec.Account.Dir)
	}
	dir := shellQuote(w.AccountDir())
	script := "umask 077 && mkdir -p " + dir + " && tar -xf - -C " + dir
	if out, err := w.shell.Run(timeout, script, bytes.NewReader(archive), true); err != nil {
		return fmt.Errorf("syncing account %q to the host failed: %s: %w", w.spec.Account.Name, strings.TrimSpace(string(out)), err)
	}
	// The version this session synced in, per file: the baseline every later
	// write-back is judged against (accountVersion). Recorded only once the host
	// holds the files.
	w.syncedVersion = versions
	return nil
}

// pullAccountBack reads the credential files back from the host and replaces
// the registry copy when one changed — a refresh the agent wrote there. Only the
// allowlisted names are accepted, whatever the host's archive contains.
func (w *sandboxWorkspace) pullAccountBack(timeout time.Duration) (changed int, err error) {
	res, err := w.writeBackAccount(timeout)
	return res.Written, err
}

// accountWriteBack is what one write-back did.
type accountWriteBack struct {
	// Written is how many credential files replaced the registry copy.
	Written int
	// Skipped names the files the host changed but that were NOT written, and why —
	// the registry moved on since this session synced it in (another session
	// refreshed it), or this process holds no sync-in baseline for it.
	Skipped []string
}

// accountVersion is the version identity of one credential file: a digest of its
// content. Content, not a timestamp or a counter: it is what the file IS, it
// cannot be spoofed by a clock, and two sessions that synced the same file hold
// the same version.
func accountVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// accountArchiveLimit is the hard byte bound on a write-back stream: every file
// the agent's scope may hold at its per-file cap, plus tar's block overhead. The
// archive is read UNDER this bound — the command is killed the moment it is
// crossed — never buffered first, so a hostile host cannot grow the daemon past it.
func accountArchiveLimit(agent string) int64 {
	n := int64(len(accountCredentialRel[agent]))
	return n*(accountSyncMaxFileBytes+2048) + 2048 + 1024
}

// writeBackAccount reads the credential files back from the host and replaces the
// registry copy — as a versioned compare-and-swap, from a bounded stream.
//
//   - BOUNDED: the host is untrusted (W7.3's own design), so its stdout is read with
//     a hard byte limit while it streams; past the limit the command is killed and
//     the write-back refused (ErrOutputOverLimit). A truncated or malformed archive
//     is refused whole, and nothing is written until every entry has parsed.
//   - VERSIONED: each file is judged against the version THIS session synced in. A
//     file the host did not change is not a refresh and is never written — a stale
//     snapshot is not news (#5864).
//   - COMPARE-AND-SWAP: a changed file replaces the registry copy only while the
//     registry still holds the version this session synced in. If another session
//     refreshed it first, or the account was removed, the write is skipped and
//     logged: a stale snapshot can never overwrite a fresher one, and a removed
//     account is never resurrected. The check and the replacement happen under one
//     lock per registry file.
func (w *sandboxWorkspace) writeBackAccount(timeout time.Duration) (accountWriteBack, error) {
	var res accountWriteBack
	apply, err := w.accountSyncApplies()
	if err != nil || !apply {
		return res, err
	}
	rels := accountCredentialRel[w.spec.Account.Agent]
	limit := accountArchiveLimit(w.spec.Account.Agent)
	quoted := make([]string, 0, len(rels))
	for _, rel := range rels {
		quoted = append(quoted, shellQuote(rel))
	}
	// Missing files are skipped, and a scope that is already gone (a repeated
	// teardown) reads back as nothing rather than as an error. head -c bounds the
	// honest case on the host's side too; the local limit is the authoritative one.
	script := "cd " + shellQuote(w.AccountDir()) + " 2>/dev/null || exit 0; " +
		"list=$(for f in " + strings.Join(quoted, " ") + "; do [ -f \"$f\" ] && printf '%s\\n' \"$f\"; done); " +
		"[ -n \"$list\" ] || exit 0; printf '%s\\n' \"$list\" | tar -cf - -T - | head -c " + strconv.FormatInt(limit+1, 10)
	var out []byte
	if ls, ok := w.shell.(limitedShell); ok {
		out, err = ls.RunLimited(timeout, script, limit)
	} else {
		out, err = w.shell.Run(timeout, script, nil, false)
		if err == nil && int64(len(out)) > limit {
			err = ErrOutputOverLimit
		}
	}
	if err != nil {
		if errors.Is(err, ErrOutputOverLimit) {
			return res, fmt.Errorf("account %q write-back refused: the host sent more than the %d-byte bound: %w", w.spec.Account.Name, limit, err)
		}
		return res, fmt.Errorf("reading account %q back from the host failed: %w", w.spec.Account.Name, err)
	}
	if len(out) == 0 {
		return res, nil
	}
	files, err := readAccountArchive(out, rels)
	if err != nil {
		return res, fmt.Errorf("account %q write-back refused: %w", w.spec.Account.Name, err)
	}
	names := make([]string, 0, len(files))
	for rel := range files {
		names = append(names, rel)
	}
	sort.Strings(names)
	for _, rel := range names {
		data := files[rel]
		synced, known := w.syncedVersion[rel]
		now := accountVersion(data)
		if known && now == synced {
			continue // the host never changed it: not a refresh
		}
		if !known {
			res.Skipped = append(res.Skipped, rel)
			log.WarningLog.Printf("account %q: not writing %s back — this process holds no sync-in baseline for it", w.spec.Account.Name, rel)
			continue
		}
		path := filepath.Join(w.spec.Account.Dir, filepath.FromSlash(rel))
		swapped, err := swapAccountCredential(path, synced, data)
		if err != nil {
			return res, fmt.Errorf("writing refreshed account %q back failed: %w", w.spec.Account.Name, err)
		}
		if !swapped {
			res.Skipped = append(res.Skipped, rel)
			log.WarningLog.Printf("account %q: not writing %s back — the registry's copy is no longer the version this session synced in (another session refreshed it, or the account was removed); its refresh stays on the host and is discarded with the session", w.spec.Account.Name, rel)
			continue
		}
		w.syncedVersion[rel] = now // later write-backs compare against what we wrote
		res.Written++
	}
	return res, nil
}

// registryLocks serializes the compare-and-swap per registry file, within the
// daemon. A write by another process (`af accounts login`) between the check and
// the atomic rename is a window of microseconds and loses at worst one refresh.
var registryLocks sync.Map // path -> *sync.Mutex

// swapAccountCredential replaces the registry file at path with data only while it
// still holds the version expected. It reports whether it wrote. A missing file is
// a mismatch: an account that was removed is never resurrected by a write-back.
func swapAccountCredential(path, expected string, data []byte) (bool, error) {
	mu, _ := registryLocks.LoadOrStore(path, &sync.Mutex{})
	l := mu.(*sync.Mutex)
	l.Lock()
	defer l.Unlock()
	cur, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if accountVersion(cur) != expected {
		return false, nil
	}
	if err := writeFileAtomic0600(path, data); err != nil {
		return false, err
	}
	return true, nil
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

func buildAccountArchive(dir string, rels []string) ([]byte, map[string]string, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	versions := map[string]string{}
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading account credential %s: %w", rel, err)
		}
		if len(data) > accountSyncMaxFileBytes {
			return nil, nil, fmt.Errorf("account credential %s is %d bytes, over the %d-byte sync bound", rel, len(data), accountSyncMaxFileBytes)
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, nil, err
		}
		// The version is taken from the SAME bytes that go on the wire, so the
		// baseline can never describe a file other than the one the host received.
		versions[rel] = accountVersion(data)
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), versions, nil
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
	if err := config.MkdirAllUnderAFHome(filepath.Dir(path), 0o700); err != nil {
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
