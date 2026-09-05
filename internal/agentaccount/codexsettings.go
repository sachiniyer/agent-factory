package agentaccount

import (
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
	_, approval := account["approval_policy"]
	_, sandbox := account["sandbox_mode"]
	if approval && sandbox {
		return nil
	}
	source, err := ambientCodexConfig()
	if err != nil {
		return err
	}
	_, ambient, err := readCodexSettings(source)
	if err != nil || ambient == nil {
		return err
	}
	if !codexHasRuntimeKeys(ambient) {
		return nil
	}
	var prefix []byte
	// Only scalar strings from this explicit allowlist can cross account homes.
	// In particular, projects and credential-store/provider configuration cannot.
	for _, key := range []string{"approval_policy", "sandbox_mode", "model"} {
		if _, exists := account[key]; exists {
			continue
		}
		value, ok := ambient[key].(string)
		if !ok {
			continue
		}
		encoded, err := toml.Marshal(map[string]string{key: value})
		if err != nil {
			return fmt.Errorf("encode codex setting %s: %w", key, err)
		}
		prefix = append(prefix, encoded...)
	}
	if len(prefix) == 0 {
		return nil
	}
	// Prepending avoids mistaking a header-looking line inside a multiline
	// string for a table and guarantees all added keys are TOP LEVEL.
	return writeAgentSettings(path, append(prefix, data...))
}

func codexHasRuntimeKeys(doc map[string]any) bool {
	_, approval := doc["approval_policy"].(string)
	_, sandbox := doc["sandbox_mode"].(string)
	return approval || sandbox
}

func codexSettingsNotice(dir string) string {
	path := filepath.Join(dir, "config.toml")
	source, err := ambientCodexConfig()
	if err != nil {
		return fmt.Sprintf("Codex runtime settings in %s were left alone: cannot locate ~/.codex/config.toml: %v", path, err)
	}
	policy := fmt.Sprintf("Registration seeds missing top-level approval_policy · sandbox_mode · model from %s into %s. Existing keys stand; unparseable documents are left alone. Credentials and project trust are never copied.", source, path)
	_, ambient, err := readCodexSettings(source)
	if err != nil {
		return policy + " Nothing was written from the ambient file because it could not be read."
	}
	if ambient == nil {
		return policy + " Nothing was written from the ambient file because it could not be parsed."
	}
	if !codexHasRuntimeKeys(ambient) {
		return policy + " Nothing was written from the ambient file because it is absent or has neither approval_policy nor sandbox_mode."
	}
	_, account, err := readCodexSettings(path)
	if err != nil || account == nil {
		return policy + " The account document could not be read or parsed and was left alone."
	}
	var present []string
	for _, key := range []string{"approval_policy", "sandbox_mode", "model"} {
		if _, ok := account[key]; ok {
			present = append(present, key+" is present")
		}
	}
	if len(present) > 0 {
		policy += " Account settings: " + strings.Join(present, " · ") + "."
	}
	return policy
}

// CodexApprovalWarning diagnoses legacy accounts without changing their policy.
// An explicitly chosen interactive policy stands just like any existing key.
func CodexApprovalWarning(name, dir string) string {
	path := filepath.Join(dir, "config.toml")
	_, doc, err := readCodexSettings(path)
	if err == nil && doc != nil {
		if _, exists := doc["approval_policy"]; exists {
			return ""
		}
	}
	reason := "has no top-level approval_policy"
	if err != nil || doc == nil {
		reason = "could not be read or parsed to verify approval_policy"
	}
	return fmt.Sprintf("Codex account %q: %s %s; sessions can stop on Codex's approval picker. Set a top-level approval_policy in that file, or run `af accounts add codex %s` to seed missing runtime settings from ~/.codex/config.toml.", name, path, reason, name)
}
