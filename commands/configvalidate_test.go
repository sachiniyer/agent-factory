package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestConfigValidateAcceptsAGoodConfig is the #2453 companion to a hand-edit:
// the config assistant edits the structured settings in the file directly, then
// runs `af config validate` to prove the file still loads before it moves on. A
// well-formed file must pass with a clear OK.
func TestConfigValidateAcceptsAGoodConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	path := filepath.Join(home, "config.toml")
	// A structured edit of exactly the shape the assistant now makes: a [theme]
	// table that af config set cannot write, hand-written into the file.
	if err := os.WriteFile(path, []byte("default_program = 'claude'\n\n[theme]\nbackground = '#101010'\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := configValidateCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("a well-formed config must validate, got error: %v", err)
	}
	if !strings.Contains(out.String(), "config OK") {
		t.Errorf("validate must report OK for a good config, got: %q", out.String())
	}
}

// TestConfigValidateRejectsABrokenEdit is the whole reason the command exists.
// A direct structured edit that does not load is a HARD startup failure with no
// fallback to defaults, so the assistant must be able to catch it before the
// user restarts. Validate has to FAIL — loudly, with a locatable error — on a
// malformed file.
func TestConfigValidateRejectsABrokenEdit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	path := filepath.Join(home, "config.toml")
	// A broken edit: an unterminated table header. This is the class of mistake a
	// hand-edit makes and af config set cannot, since set never regenerates the
	// file from a bad state.
	if err := os.WriteFile(path, []byte("default_program = 'claude'\n[theme\nbackground = '#101010'\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := configValidateCmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("validate must FAIL on a config that does not load — an unvalidated broken edit wedges startup")
	}
	if strings.Contains(out.String(), "config OK") {
		t.Errorf("a broken config must not print OK, got stdout: %q", out.String())
	}
}

// TestConfigValidateAcceptsAMissingConfig pins that first run is not a failure:
// there is no config file yet, af materializes defaults on first start, and the
// assistant running validate before anything exists must not report an error.
func TestConfigValidateAcceptsAMissingConfig(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := configValidateCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("a missing config is first-run, not an error, got: %v", err)
	}
	if !strings.Contains(out.String(), "config OK") {
		t.Errorf("a missing config should still report OK, got: %q", out.String())
	}
}

// TestConfigValidateDoesNotMutateTheConfig is the read-only promise that matters.
// `af config set` materializes and secures the home; validate must not touch the
// config it checks — a check that rewrites what it checks is not one. Assert the
// config file is byte-for-byte unchanged and no second config file (a
// materialized default, a converted config.json) appeared.
//
// It deliberately does NOT assert the whole home is untouched: every command runs
// log.Initialize, which writes agent-factory.log into the home. That is not a
// config mutation, so the assertion is scoped to config files rather than a
// directory-entry count that the log file would trip.
func TestConfigValidateDoesNotMutateTheConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, "config.toml")
	jsonPath := filepath.Join(home, "config.json")
	original := []byte("default_program = 'codex'\n\n[theme]\nbackground = '#202020'\n")
	if err := os.WriteFile(tomlPath, original, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := configValidateCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("validate: %v", err)
	}

	after, _ := os.ReadFile(tomlPath)
	if string(after) != string(original) {
		t.Errorf("validate changed the config it checked.\n got: %q\nwant: %q", after, original)
	}
	if _, err := os.Stat(jsonPath); err == nil {
		t.Error("validate materialized a second config file (config.json) — it must read, never write")
	}
}
