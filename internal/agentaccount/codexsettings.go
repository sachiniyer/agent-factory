package agentaccount

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

func ambientCodexConfig() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// readCodexSettings keeps the original bytes: re-encoding the whole document
// would unnecessarily rewrite comments and settings owned by the operator.
func readCodexSettings(path string) ([]byte, map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, map[string]any{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &doc); err != nil {
		return data, nil, nil
	}
	return data, doc, nil
}

func answerCodexLoginPrompts(dir string) error {
	path := filepath.Join(dir, "config.toml")
	data, account, err := readCodexSettings(path)
	if err != nil || account == nil {
		return err
	}
	source, err := ambientCodexConfig()
	if err != nil {
		return nil // Ambient defaults are optional; the login notice reports this.
	}
	_, ambient, err := readCodexSettings(source)
	if err != nil || ambient == nil {
		return nil
	}
	effective, reason := codexRuntimeSource(ambient)
	if reason != "" {
		return nil
	}
	if !codexHasRuntimeKeys(effective) {
		return nil
	}
	accountEffective, accountReason := codexEffectiveSettings(account)
	if accountReason != "" {
		accountEffective = account
	}
	var prefix []byte
	// Provider-specific models cannot be interpreted without provider configuration,
	// which must not cross account homes. Presence matters, regardless of value.
	ambientModelBlock := codexModelSeedBlockReason(ambient)
	accountModelBlock := codexModelSeedBlockReason(account)
	for _, key := range []string{"approval_policy", "sandbox_mode", "model"} {
		if key == "model" && (ambientModelBlock != "" || accountModelBlock != "") {
			continue
		}
		if _, exists := accountEffective[key]; exists {
			continue
		}
		value, exists := effective[key]
		if !exists {
			continue
		}
		if key == "approval_policy" {
			value = codexApprovalSeedValue(value)
		} else if _, ok := value.(string); !ok {
			continue
		}
		encoded, err := encodeCodexSetting(key, value)
		if err != nil {
			return fmt.Errorf("encode codex setting %s: %w", key, err)
		}
		prefix = append(prefix, encoded...)
	}
	if accountReason == "" {
		options, err := codexWorkspaceOptions(effective, accountEffective)
		if err != nil {
			return err
		}
		prefix = append(prefix, options...)
	}
	if len(prefix) == 0 {
		return nil
	}
	// Prepending avoids mistaking a header-looking line inside a multiline
	// string for a table and guarantees all added keys are TOP LEVEL.
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		prefix = append([]byte("\xef\xbb\xbf"), prefix...)
		data = data[3:]
	}
	return writeAgentSettings(path, append(prefix, data...))
}

// codexWorkspaceOptions copies only the known non-credential sandbox options.
// Inline TOML keeps the table after the seeded scalar keys and before the original
// bytes without moving original top-level keys into a newly opened table scope.
func codexWorkspaceOptions(ambient, account map[string]any) ([]byte, error) {
	if mode, _ := ambient["sandbox_mode"].(string); mode != "workspace-write" {
		return nil, nil
	}
	// A missing account mode is seeded from ambient in this same pass.
	mode, exists := account["sandbox_mode"]
	if accountMode, _ := mode.(string); exists && accountMode != "workspace-write" {
		return nil, nil
	}
	if _, exists := account["sandbox_workspace_write"]; exists {
		return nil, nil
	}
	table, ok := ambient["sandbox_workspace_write"].(map[string]any)
	if !ok {
		return nil, nil
	}
	options := make(map[string]any)
	for _, key := range []string{"network_access", "writable_roots", "exclude_tmpdir_env_var", "exclude_slash_tmp"} {
		if value, exists := table[key]; exists {
			options[key] = value
		}
	}
	return encodeCodexSetting("sandbox_workspace_write", options)
}

// encodeCodexSetting keeps every added setting at top level, including policies
// and sandbox options whose values are tables.
func encodeCodexSetting(key string, value any) ([]byte, error) {
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).SetTablesInline(true).Encode(map[string]any{key: value}); err != nil {
		return nil, fmt.Errorf("encode codex setting %s: %w", key, err)
	}
	return encoded.Bytes(), nil
}

func codexHasRuntimeKeys(doc map[string]any) bool {
	approval := validCodexApprovalPolicy(doc["approval_policy"])
	sandbox := validCodexSandboxMode(doc["sandbox_mode"])
	return approval || sandbox
}

func codexSettingsNotice(dir string) string {
	path := filepath.Join(dir, "config.toml")
	source, err := ambientCodexConfig()
	if err != nil {
		return fmt.Sprintf("Nothing was written to %s because ~/.codex/config.toml could not be read: cannot locate the home directory: %v", path, err)
	}
	policy := fmt.Sprintf("Registration seeds missing approval_policy · sandbox_mode · model from the effective settings in %s into %s: the selected profile overrides root values. Existing keys stand, including selected-profile values. Models are not seeded across provider selections. Profiles and provider configuration are never copied. Workspace options are copied only for an effective workspace-write account. Unresolved ambient profiles or invalid approval/sandbox settings (including active workspace options) skip all seeding. Unparseable documents are left alone; credentials and project trust are never copied.", source, path)
	_, ambient, err := readCodexSettings(source)
	if err != nil {
		return policy + " Nothing was written from the ambient file because it could not be read."
	}
	if ambient == nil {
		return policy + " Nothing was written from the ambient file because it could not be parsed."
	}
	effective, reason := codexRuntimeSource(ambient)
	if reason != "" {
		return policy + " Nothing was written from the ambient file because " + strings.TrimSuffix(reason, ".") + "."
	}
	if reason := codexModelSeedBlockReason(ambient); reason != "" {
		policy += " model not seeded: ~/.codex/config.toml " + strings.TrimSuffix(reason, ".") + "."
	}
	if !codexHasRuntimeKeys(effective) {
		return policy + " Nothing was written from the ambient file because it is absent or has neither approval_policy nor sandbox_mode."
	}
	_, account, err := readCodexSettings(path)
	if err != nil || account == nil {
		return policy + " The account document could not be read or parsed and was left alone."
	}
	if reason := codexModelSeedBlockReason(account); reason != "" {
		policy += " model not seeded: this account's config.toml " + strings.TrimSuffix(reason, ".") + "."
	}
	var present []string
	for _, key := range []string{"approval_policy", "sandbox_mode", "model", "sandbox_workspace_write"} {
		if _, ok := account[key]; ok {
			present = append(present, key+" is present")
		}
	}
	if len(present) > 0 {
		policy += " Account settings: " + strings.Join(present, " · ") + "."
	}
	return policy
}
