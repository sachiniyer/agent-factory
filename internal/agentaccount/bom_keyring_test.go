package agentaccount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexKeyringCollapse_BOMPrefixedConfigIsRefused(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "codex", "work")
	if err != nil {
		t.Fatalf("register account: %v", err)
	}
	config := "\xef\xbb\xbfmodel = \"gpt-5\"\ncli_auth_credentials_store = \"keyring\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write account config: %v", err)
	}
	_, err = CheckLoginPreconditions("codex", dir)
	if err == nil {
		t.Fatal("a BOM-prefixed keyring-backed codex account was accepted for login — the BOM blinded the guard")
	}
	for _, want := range []string{"cli_auth_credentials_store", "keyring", "config.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
}
