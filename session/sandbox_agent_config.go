package session

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Operator-selected agent config (#5571 W7.7).
//
// Agent personalization — claude's settings.json, the user CLAUDE.md and its MCP
// config, codex's config.toml and AGENTS.md — is not a credential, and it is not
// carried by the account sync, so a sandbox session otherwise runs a bare agent.
// The operator declares which of it to carry with the global-only
// `sandbox_agent_config` key, and af places exactly that:
//
//   - from the operator's own agent home on the daemon box, read by af;
//   - into the session's own account scope (<sessionDir>/account), beside the
//     synced credentials and nowhere else — never the remote user's global config;
//   - only on a host in the global host list, and only for an account-scoped
//     session, because the account scope is what points the agent at that
//     directory. The same licence that gates the credentials gates this: it is
//     ProvisionSpec.AccountSyncHost, which no repository can set.
//
// The set of files is closed (config.NormalizeSandboxAgentConfig), so the key can
// never name a path off the daemon box, and the transfer is one-way: nothing is
// written back.

// sandboxAgentConfigEntries reports the files this provision places operator config:
// an account-scoped session on a licensed host with declared entries for its agent.
func (w *sandboxWorkspace) sandboxAgentConfigEntries() []string {
	if len(w.spec.AgentConfigFiles) == 0 || w.spec.Account.Dir == "" ||
		strings.TrimSpace(w.spec.AccountSyncHost) == "" {
		return nil
	}
	var files []string
	for _, entry := range w.spec.AgentConfigFiles {
		agent, file, ok := strings.Cut(entry, ":")
		if ok && agent == w.spec.Account.Agent {
			files = append(files, file)
		}
	}
	return files
}

// agentConfigMaxFileBytes bounds one operator-declared personalization read, one
// way: from the operator's own agent home on the daemon box, the transfer this
// feature makes — and, because every selected file lands in the single retained
// archive buffer in syncAgentConfigInto, it bounds the archive total as well (a
// file that would overflow it is skipped like an unreadable one). It is NOT the
// credential-sync bound (accountSyncMaxFileBytes), which defends a write-back
// FROM an untrusted host's archive: claude's .claude.json keeps per-project
// prompt history and grows past a megabyte for an active operator, so the file
// this feature exists to carry was silently dropped under the credential bound.
// Sized for that growth, and still bounded so runaway or hostile operator files
// cannot fill the daemon's memory.
const agentConfigMaxFileBytes = 64 << 20

// syncAgentConfigIn places the declared files in the session's account scope. A
// declared file that does not exist on the daemon box is skipped with a warning —
// the operator's declaration must not fail a create — and nothing is sent when no
// file resolves. The returned skip list is the user-visible form of that
// warning (#6337): every file the placement refused, named as the operator
// declared it, with the reason.
func (w *sandboxWorkspace) syncAgentConfigIn(timeout time.Duration) ([]string, error) {
	return w.syncAgentConfigInto(timeout, w.AccountDir())
}

// syncAgentConfigInto places the declared files in scope dir on the host: the live
// account scope at provision, and the incoming scope during an account handoff, so
// the personalization survives the swap exactly as it survives a restart (the
// rotation replaces the whole live directory with the staged one).
//
// Each skipped file is logged AND returned as one "agent:file: <reason>" entry —
// the daemon log warns, but a session row that can never name its skip is a
// session that appears fully provisioned while running a bare agent (#6337).
// The entry leads with the declaration the operator wrote; the reason beside
// it can name the resolved path, which is why bugreport redacts the field.
func (w *sandboxWorkspace) syncAgentConfigInto(timeout time.Duration, scopeDir string) ([]string, error) {
	files := w.sandboxAgentConfigEntries()
	if len(files) == 0 {
		return nil, nil
	}
	agent := w.spec.Account.Agent
	home := operatorAgentHome(agent)
	if home == "" {
		return nil, nil
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var skipped []string
	sent := 0
	total := 0
	for _, file := range files {
		skip := func(reason error) {
			entry := fmt.Sprintf("%s:%s: %v", agent, file, reason)
			log.WarningLog.Printf("sandbox_agent_config: skipping %s", entry)
			skipped = append(skipped, entry)
		}
		data, err := readOperatorAgentFile(agent, home, file)
		if err != nil {
			skip(err)
			continue
		}
		if total+len(data) > agentConfigMaxFileBytes {
			skip(fmt.Errorf("%d bytes would take the archive over the %d-byte bound", len(data), agentConfigMaxFileBytes))
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: file, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
		total += len(data)
		sent++
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if sent == 0 {
		return skipped, nil
	}
	dir := shellQuote(scopeDir)
	script := "umask 077 && mkdir -p " + dir + " && tar -xf - -C " + dir
	if out, err := w.shell.Run(timeout, script, bytes.NewReader(buf.Bytes()), true); err != nil {
		return nil, fmt.Errorf("placing the operator's %s config on the host failed: %s: %w", agent, strings.TrimSpace(string(out)), err)
	}
	return skipped, nil
}

// operatorAgentHome is the operator's own agent home on THIS machine — the place
// the declared files are read from: the agent's own relocation variable when set,
// else its default home directory.
func operatorAgentHome(agent string) string {
	switch agent {
	case tmux.ProgramClaude:
		if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
			return d
		}
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, ".claude")
		}
	case tmux.ProgramCodex:
		if d := strings.TrimSpace(os.Getenv("CODEX_HOME")); d != "" {
			return d
		}
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, ".codex")
		}
	}
	return ""
}

// readOperatorAgentFile reads one declared file, bounded, and rewrites it where a
// copy would defeat the session's account scope.
func readOperatorAgentFile(agent, home, file string) ([]byte, error) {
	path := filepath.Join(home, file)
	// claude keeps its MCP config beside the config dir's PARENT when no config
	// dir is relocated: ~/.claude.json, not ~/.claude/.claude.json.
	if agent == tmux.ProgramClaude && file == ".claude.json" && os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		path = filepath.Join(filepath.Dir(home), ".claude.json")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > agentConfigMaxFileBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d-byte bound", path, info.Size(), agentConfigMaxFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if agent == tmux.ProgramCodex && file == "config.toml" {
		if data, err = forceCodexFileCredentialStore(data); err != nil {
			return nil, err
		}
	}
	return data, nil
}

const codexCredStoreKey = "cli_auth_credentials_store"

// forceCodexFileCredentialStore makes a copied codex config.toml use the
// file-based credential store. A config carrying
// `cli_auth_credentials_store = "keyring"` makes codex ignore the account's
// auth.json and use the MACHINE-WIDE identity, so every account would silently
// authenticate as the same person (the hazard sessionenv.Account documents).
//
// The file is PARSED, not pattern-matched: TOML spells one key many ways (bare,
// "quoted", 'literal', with escapes such as "cli_auth_\u0063redentials_store",
// dotted, inside a [table] or an inline table), and a text match covers only the
// ones it lists. The key is removed from every table at any depth and the
// top-level setting is set to "file". A file that does not parse is refused, never
// copied through unexamined. The re-encoded file keeps every value but not the
// operator's comments or layout.
func forceCodexFileCredentialStore(data []byte) ([]byte, error) {
	var tree map[string]any
	if err := toml.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("codex config.toml does not parse as TOML, so it is not copied: %w", err)
	}
	if tree == nil {
		tree = map[string]any{}
	}
	dropCodexCredStoreKey(tree)
	tree[codexCredStoreKey] = "file"
	out, err := toml.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("re-encoding codex config.toml: %w", err)
	}
	return out, nil
}

func dropCodexCredStoreKey(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, codexCredStoreKey)
		for _, child := range t {
			dropCodexCredStoreKey(child)
		}
	case []any:
		for _, child := range t {
			dropCodexCredStoreKey(child)
		}
	}
}
