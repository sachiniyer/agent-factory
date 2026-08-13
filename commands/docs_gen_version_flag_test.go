package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIReferenceIncludesRootVersionFlag covers #3227: NewRootCommand sets
// rootCmd.Version, which makes cobra provide `af --version`/`-v` — but cobra
// registers that flag lazily inside Execute, and only on the command being
// executed. `af gen-docs` executes a subcommand, so without eager registration
// the root flag set walked by writeCLIReference holds no version flag and the
// generated reference omits it while promising to list every flag.
func TestCLIReferenceIncludesRootVersionFlag(t *testing.T) {
	origVersion := version
	origRootVersion := rootCmd.Version
	t.Cleanup(func() {
		version = origVersion
		rootCmd.Version = origRootVersion
	})

	_ = NewRootCommand(Options{Version: "9.9.9"})

	path := filepath.Join(t.TempDir(), "cli.md")
	if err := writeCLIReference(path); err != nil {
		t.Fatalf("writeCLIReference: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated reference: %v", err)
	}
	if !strings.Contains(string(data), "`-v`, `--version`") {
		t.Fatal("generated CLI reference omits the root --version flag row")
	}
}
