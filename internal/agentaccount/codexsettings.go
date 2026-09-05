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
	if err := toml.Unmarshal(data, &doc); err != nil {
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
	if policy, exists := ambient["approval_policy"]; exists && !validCodexApprovalPolicy(policy) {
		return nil // Refuse partial inheritance from an unverified policy.
	}
	if !codexHasRuntimeKeys(ambient) {
		return nil
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
		if _, exists := account[key]; exists {
			continue
		}
		value, exists := ambient[key]
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
	options, err := codexWorkspaceOptions(ambient, account)
	if err != nil {
		return err
	}
	prefix = append(prefix, options...)
	if len(prefix) == 0 {
		return nil
	}
	// Prepending avoids mistaking a header-looking line inside a multiline
	// string for a table and guarantees all added keys are TOP LEVEL.
	return writeAgentSettings(path, append(prefix, data...))
}

// codexWorkspaceOptions copies only the known non-credential sandbox options.
// Inline TOML keeps the table after the seeded scalar keys and before the original
// bytes without moving original top-level keys into a newly opened table scope.
func codexWorkspaceOptions(ambient, account map[string]any) ([]byte, error) {
	if mode, _ := ambient["sandbox_mode"].(string); mode != "workspace-write" {
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
	_, sandbox := doc["sandbox_mode"].(string)
	return approval || sandbox
}

func codexSettingsNotice(dir string) string {
	path := filepath.Join(dir, "config.toml")
	source, err := ambientCodexConfig()
	if err != nil {
		return fmt.Sprintf("Nothing was written to %s because ~/.codex/config.toml could not be read: cannot locate the home directory: %v", path, err)
	}
	policy := fmt.Sprintf("When the ambient file has approval_policy or sandbox_mode, registration independently seeds missing top-level approval_policy · sandbox_mode · model from %s into %s. Model is seeded only when neither document nor its selected profile has model_provider and selected profiles can be verified. For ambient workspace-write mode, an absent sandbox_workspace_write table is copied with only these options (network_access · writable_roots · exclude_tmpdir_env_var · exclude_slash_tmp). Approval policies are validated before seeding; an invalid ambient policy skips all seeding. Existing keys stand; unparseable documents are left alone. Credentials, provider configuration and project trust are never copied.", source, path)
	_, ambient, err := readCodexSettings(source)
	if err != nil {
		return policy + " Nothing was written from the ambient file because it could not be read."
	}
	if ambient == nil {
		return policy + " Nothing was written from the ambient file because it could not be parsed."
	}
	if policyValue, exists := ambient["approval_policy"]; exists && !validCodexApprovalPolicy(policyValue) {
		return policy + " Nothing was written from the ambient file because approval_policy could not be verified. " + codexApprovalPolicyNotice
	}
	if reason := codexModelSeedBlockReason(ambient); reason != "" {
		policy += " model not seeded: ~/.codex/config.toml " + reason + "."
	}
	if !codexHasRuntimeKeys(ambient) {
		return policy + " Nothing was written from the ambient file because it is absent or has neither approval_policy nor sandbox_mode."
	}
	_, account, err := readCodexSettings(path)
	if err != nil || account == nil {
		return policy + " The account document could not be read or parsed and was left alone."
	}
	if reason := codexModelSeedBlockReason(account); reason != "" {
		policy += " model not seeded: this account's config.toml " + reason + "."
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
