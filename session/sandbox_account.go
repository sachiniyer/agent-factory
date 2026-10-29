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
	archive, versions, err := buildAccountArchive(w.spec.Account.Dir, accountCredentialRel[w.spec.Account.Agent], accountScopeSeed(w.spec.Account.Agent))
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

// tarBlock and tarRecord are tar's real arithmetic: an archive is a sequence of
// 512-byte blocks — a header block, the data rounded UP to a block, and a trailer of
// two zero blocks — and the whole archive is then padded to a multiple of the
// RECORD size. GNU tar's default blocking factor is 20 (a 10,240-byte record); a
// writer told `-b 1` pads to 512 instead. The bound below holds for either.
const (
	tarBlock  = 512
	tarRecord = 20 * tarBlock
)

func roundUp(n, to int64) int64 { return (n + to - 1) / to * to }

// tarArchiveSize is the exact size tar writes for regular files of the given sizes
// (names short enough for a plain header, which every credential name is), padded to
// record bytes.
func tarArchiveSize(sizes []int64, record int64) int64 {
	var n int64
	for _, size := range sizes {
		n += tarBlock + roundUp(size, tarBlock)
	}
	n += 2 * tarBlock // the end-of-archive marker
	return roundUp(n, record)
}

// accountArchiveLimit is the hard byte bound on a write-back stream: the LARGEST
// archive an honest tar can produce for this agent's credential files, every one at
// its per-file cap, padded to a full default record. A credential of exactly the
// cap therefore fits (#5901) — the bound is tar's own arithmetic, not an estimate —
// and anything larger is the host misbehaving. The archive is read UNDER this
// bound, never buffered first, so a hostile host cannot grow the daemon past it.
func accountArchiveLimit(agent string) int64 {
	sizes := make([]int64, len(accountCredentialRel[agent]))
	for i := range sizes {
		sizes[i] = accountSyncMaxFileBytes
	}
	return tarArchiveSize(sizes, tarRecord)
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
	return w.writeBackAccountFrom(w.AccountDir(), timeout)
}

// writeBackAccountFrom is writeBackAccount for a scope directory other than the
// live one: the account handoff reads the OUTGOING account from the directory it
// was rotated to, with the same allowlist, bound, validation and CAS.
func (w *sandboxWorkspace) writeBackAccountFrom(dir string, timeout time.Duration) (accountWriteBack, error) {
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
	// teardown) reads back as nothing rather than as an error. tar is the LAST
	// stage, so the command's exit status is tar's own: a file that shrank while it
	// was read ("padding with zeros", exit 1) fails the whole read instead of
	// arriving as a structurally valid archive (#5902). There is no limiter in the
	// pipeline to mask it — the bound is enforced locally, as the stream is read.
	script := "cd " + shellQuote(dir) + " 2>/dev/null || exit 0; " +
		"list=$(for f in " + strings.Join(quoted, " ") + "; do [ -f \"$f\" ] && printf '%s\\n' \"$f\"; done); " +
		"[ -n \"$list\" ] || exit 0; printf '%s\\n' \"$list\" | tar -cf - -T -"
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

func buildAccountArchive(dir string, rels []string, seed map[string][]byte) ([]byte, map[string]string, error) {
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
	// Minimal per-agent scope seed — never a credential, and never read from the
	// account's own settings.
	seedNames := make([]string, 0, len(seed))
	for name := range seed {
		seedNames = append(seedNames, name)
	}
	sort.Strings(seedNames)
	for _, name := range seedNames {
		data := seed[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, nil, err
		}
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
	// The archive names are UNTRUSTED. They are only ever used to LOOK UP an entry in
	// the allowlist: the map the caller receives is keyed by the allowlisted constant
	// that matched, never by a string derived from the archive's own header, so no
	// archive-controlled value can reach a filesystem path (the go/zipslip class).
	canonical := make(map[string]string, len(allowed))
	for _, a := range allowed {
		if !filepath.IsLocal(a) {
			return nil, fmt.Errorf("internal: allowlisted credential name %q is not a local path", a)
		}
		canonical[a] = a
	}
	// A complete archive ends in tar's end-of-archive marker. One cut off at a
	// block boundary after a whole entry would otherwise read as a clean, shorter
	// archive.
	if !hasTarTrailer(data) {
		return nil, fmt.Errorf("unreadable archive: it does not end with tar's end-of-archive marker (truncated)")
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
		name, listed := canonical[strings.TrimPrefix(h.Name, "./")]
		if !listed || h.Typeflag != tar.TypeReg {
			// A traversal ("../x"), an absolute path, a link, or any name that is not
			// exactly an allowlisted credential file: the whole archive is refused.
			return nil, fmt.Errorf("unexpected archive entry %q", h.Name)
		}
		if !filepath.IsLocal(name) {
			return nil, fmt.Errorf("unexpected archive entry %q", h.Name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("archive entry %q appears twice", h.Name)
		}
		if h.Size > accountSyncMaxFileBytes {
			return nil, fmt.Errorf("archive entry %q is over the %d-byte bound", h.Name, accountSyncMaxFileBytes)
		}
		b, err := io.ReadAll(io.LimitReader(tr, accountSyncMaxFileBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(b)) != h.Size {
			return nil, fmt.Errorf("archive entry %q holds %d bytes but its header says %d", h.Name, len(b), h.Size)
		}
		if err := validateCredentialContent(name, b); err != nil {
			return nil, err
		}
		out[name] = b
	}
}

// hasTarTrailer reports whether data ends with at least the two zero blocks tar
// writes to close an archive (record padding only adds more zeros).
func hasTarTrailer(data []byte) bool {
	if len(data) < 2*tarBlock {
		return false
	}
	for _, c := range data[len(data)-2*tarBlock:] {
		if c != 0 {
			return false
		}
	}
	return true
}

// validateCredentialContent refuses content that cannot be a credential a real
// agent wrote: empty (a file truncated to nothing would erase a valid login), or
// carrying a NUL byte (zero padding where the file shrank under tar — every
// credential file is text). It is the last gate before the registry is touched, and
// an independent one: it does not trust the transport's exit status.
func validateCredentialContent(name string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("archive entry %q is empty; refusing to replace a credential with nothing", name)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("archive entry %q contains NUL bytes (a file that shrank while it was read); refusing to replace a credential with corrupted content", name)
	}
	return nil
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

// accountScrubEnv is the environment-file text that defines every identity
// variable of the session's agent EMPTY on the host (#5571 W7.4). The host's
// launch environment is the host user's: an ambient ANTHROPIC_API_KEY or
// OPENAI_API_KEY outranks the synced OAuth credentials, so without this the
// session would report one account while spending another. The daemon cannot
// subtract from an environment it never sees, so the names are defined empty in
// the 0600 identity file the launch shell sources — the docker backend's shape.
func (w *sandboxWorkspace) accountScrubEnv() string {
	var b strings.Builder
	for _, name := range w.accountPinnedNames() {
		b.WriteString(name + "=\n")
	}
	return b.String()
}

// accountPinnedNames are the variables an account-scoped remote launch defines
// EMPTY: every identity name (credentials plus late-environment names) AND every
// cloud-authentication selector of the agent, never the config variable itself.
// Both come from sessionenv's per-agent tables — the same source the local path and
// docker use — so the paths cannot drift.
//
// The selectors matter as much as the keys. A cloud mode (Bedrock, Vertex, Foundry)
// on the HOST would route the agent to that provider's credentials and billing
// while the session reports the selected account. The local path REFUSES an active
// selector (ApplyAccount); the daemon cannot see the host's environment to refuse,
// so the remote path NEUTRALIZES it — an empty selector is disabled, which also
// stops the filters from admitting the provider credentials it gates.
func (w *sandboxWorkspace) accountPinnedNames() []string {
	configVar, _, ok := w.accountEnv()
	if !ok {
		return nil
	}
	agent := w.spec.Account.Agent
	seen := map[string]bool{configVar: true}
	var names []string
	add := func(list []string) {
		for _, name := range list {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	add(sessionenv.AccountIdentityNames(agent))
	add(sessionenv.AgentAuthSelectors(agent))
	sort.Strings(names)
	return names
}

// accountLaunchGuard is the shell fragment that makes the agent-server launch FAIL
// CLOSED unless the session's own synced scope is in place: the agent's config
// variable must equal this session's scope exactly — never the host user's global
// agent home, never another session's scope — and the scope must hold at least one
// of the account's credential files. It runs after the identity file is sourced,
// so it judges the environment the agent-server will actually inherit. Empty when
// the session carries no account.
func (w *sandboxWorkspace) accountLaunchGuard() string {
	name, scope, ok := w.accountEnv()
	if !ok {
		return ""
	}
	var has []string
	for _, rel := range accountCredentialRel[w.spec.Account.Agent] {
		has = append(has, "[ -f "+shellQuote(scope+"/"+rel)+" ]")
	}
	// Every cloud selector must be empty in the environment the agent-server will
	// inherit, whatever the host's own environment said (accountPinnedNames).
	selectorsEmpty := ""
	for _, selector := range sessionenv.AgentAuthSelectors(w.spec.Account.Agent) {
		selectorsEmpty += fmt.Sprintf(" && [ -z \"${%s:-}\" ]", selector)
	}
	return fmt.Sprintf("{ [ \"$%s\" = %s ] && { %s; }%s; } || { echo %s >&2; exit 1; }; ",
		name, shellQuote(scope), strings.Join(has, " || "), selectorsEmpty,
		shellQuote("af: the session's account scope is not in place; refusing to start the agent under any other identity"))
}

// validateRemoteAccountProgram applies the account boundary's command-shape half to
// the program a sandbox will run, with the daemon's own resolution — the program the
// agent-server is told to launch. It is the docker path's check, for the same
// reason: af must never hand an account's credentials to a program it did not
// choose, and a command that sets an identity variable itself, or that af cannot
// prove is a direct invocation of the agent, does not qualify.
//
// The executable resolves on the HOST, a filesystem the daemon cannot inspect, so
// af's own built-in executable identity is NOT granted across that boundary (a bare
// agent name is independently provable; a daemon-qualified path fails closed). The
// ambient environment is empty here by design: the host's own is scrubbed at launch
// (accountScrubEnv), and the daemon's says nothing about it.
func validateRemoteAccountProgram(cfg *config.ResolvedConfig, spec *ProvisionSpec) error {
	// What the sandbox is told to launch: the daemon's configured program with af's
	// own auto-detected executable path rewritten to the bare agent name (W2.1) —
	// the daemon's path means nothing on another filesystem. Whether the ORIGINAL
	// command is af's built-in detected override is what vouches for the one
	// argument af appended to it.
	original := config.ResolveProgram(&cfg.Config, spec.Program)
	resolved := remoteProgramForHost(cfg, spec.Program)
	if err := refuseAccountAgentDrift(spec.Account.Name, spec.Account.Agent, resolved); err != nil {
		return fmt.Errorf("backend=ssh: %w", err)
	}
	proof := accountLaunchProof(resolved, resolved, builtInProgramOverride(cfg, spec.Program, original), nil, "")
	hostExecutable := proof.TrustedExecutable
	spec.Account.TrustedExecutable = ""
	spec.Account.GeneratedArgs = proof.GeneratedArgs
	if _, err := sessionenv.ApplyAccount([]string{}, resolved, spec.Account); err != nil {
		if hostExecutable != "" && sessionenv.IsAccountCommandValidationError(err) {
			return fmt.Errorf("backend=ssh: resolved program names host executable %q, whose identity cannot be established on the sandbox host: %w", hostExecutable, err)
		}
		return fmt.Errorf("backend=ssh: %w", err)
	}
	return nil
}

// accountPassthroughNames are the variables an account-scoped remote launch must
// carry through the environment filters: the scoped config variable and every
// identity name accountScrubEnv defines empty, derived from sessionenv's per-agent
// tables rather than listed here. Empty for a session with no account.
func (w *sandboxWorkspace) accountPassthroughNames() []string {
	configVar, _, ok := w.accountEnv()
	if !ok {
		return nil
	}
	return append([]string{configVar}, w.accountPinnedNames()...)
}

// accountScopeSeed is the minimal, non-credential configuration a fresh account
// scope needs to start without asking for input. Gemini shows its authentication
// picker unless security.auth.selectedType is set, and af sets it when it registers
// an account locally (agentaccount's selectGeminiGoogleLogin); a scope seeded with
// only credential files would wait for a manual choice on every provision. Only the
// Google-login selection is written — an API-key or Vertex selection is an ambient
// identity af never chooses — and nothing else from the account's settings.
func accountScopeSeed(agent string) map[string][]byte {
	if agent != tmux.ProgramGemini {
		return nil
	}
	return map[string][]byte{
		".gemini/settings.json": []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}` + "\n"),
	}
}

// AccountScopeAgentEnv names the variable the launch's identity file sets for an
// account-scoped sandbox: the agent whose account the synced scope belongs to. The
// in-sandbox agent-server reads it to know it must prove executable attribution
// before it starts anything. It is carried to the agent-server only — never named
// to the pane filter — so no agent ever sees it.
const AccountScopeAgentEnv = "AF_ACCOUNT_SCOPE_AGENT"

// RequireAttributedAccountProgram is the host-side half of "af never hands an
// account to a program it did not choose" (#5571 W7.4). Proving the command's
// SHAPE is not enough when the binary that runs is a different one: a bare name is
// resolved on the host, and a relative PATH entry (a repository's own bin/) makes
// that lookup undecidable until the pane's working directory is known — so the
// resolver leaves the name bare and the pane's shell would pick whatever the
// workspace contains.
//
// An account-scoped agent-server therefore starts only when the resolved command's
// executable is an ABSOLUTE path to an executable file named for the agent — the
// exact path the pane will exec — and otherwise refuses, before any pane exists
// and before the scope can reach one. The host user's own absolute PATH entries are
// their choice (sessions on one host share its user, settled decision 3); a
// workspace-relative entry is not.
func RequireAttributedAccountProgram(command, agent string) error {
	exe, err := sessionenv.LocateProgramExecutable(command)
	if err != nil || exe.Opaque || exe.PATHDynamic {
		return fmt.Errorf("account-scoped %s: af cannot attribute the executable of %q, so the account is not passed to it", agent, command)
	}
	name := exe.Name
	if !strings.HasPrefix(name, "/") {
		return fmt.Errorf("account-scoped %s: %q did not resolve to an absolute path on this host (a relative or empty PATH entry, such as a repository's bin/, is searched first), so af cannot attribute the executable and does not pass the account to it; "+
			"put an absolute directory first on the login PATH or remove the relative entry", agent, name)
	}
	if filepath.Base(name) != agent {
		return fmt.Errorf("account-scoped %s: the resolved executable %q is not named %q, so af cannot attribute it", agent, name, agent)
	}
	if !canExecuteFile(name) {
		return fmt.Errorf("account-scoped %s: the resolved executable %q is not an executable file for this user", agent, name)
	}
	return nil
}
