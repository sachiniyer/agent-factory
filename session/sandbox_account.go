package session

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
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
	archive, n, err := buildAccountArchive(w.spec.Account.Dir, accountCredentialRel[w.spec.Account.Agent], accountScopeSeed(w.spec.Account.Agent))
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

func buildAccountArchive(dir string, rels []string, seed map[string][]byte) ([]byte, int, error) {
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
			return nil, 0, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, 0, err
		}
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
	configVar, _, ok := w.accountEnv()
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, name := range sessionenv.AccountIdentityNames(w.spec.Account.Agent) {
		if name == configVar {
			// Injected with the scope, never emptied: an empty copy would erase the
			// selection this exists to make.
			continue
		}
		b.WriteString(name + "=\n")
	}
	return b.String()
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
	return fmt.Sprintf("{ [ \"$%s\" = %s ] && { %s; }; } || { echo %s >&2; exit 1; }; ",
		name, shellQuote(scope), strings.Join(has, " || "),
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
	names := []string{configVar}
	for _, name := range sessionenv.AccountIdentityNames(w.spec.Account.Agent) {
		if name != configVar {
			names = append(names, name)
		}
	}
	return names
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
