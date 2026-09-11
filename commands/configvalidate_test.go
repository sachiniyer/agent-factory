package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/spf13/cobra"
)

// TestConfigValidateAcceptsAGoodConfig is the companion to a raw hand-edit.
// Every config-set surface validates before writing, but users retain direct
// ownership of config.toml and need a clear OK after editing it themselves.
func TestConfigValidateAcceptsAGoodConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	path := filepath.Join(home, "config.toml")
	// A structured hand-edit remains valid even though the safer UI/CLI path can
	// now write this same table.
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
// A direct edit that does not load is a HARD startup failure with no fallback
// to defaults. Validate has to FAIL — loudly, with a locatable error — on a
// malformed file.
func TestConfigValidateRejectsABrokenEdit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	path := filepath.Join(home, "config.toml")
	// A broken edit: an unterminated table header. This is the class of mistake a
	// hand-edit can make and af config set cannot, since set refuses a bad state.
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

// TestConfigValidateAcceptsAnEmptyStub is the fix's headline guarantee for this
// command: a contentless config.toml (here zero bytes) with no shadowing
// config.json is a state af boots on — startup removes the stub and materializes
// defaults — so validate, whose doc-comment claims to run "the same parse+validate
// af runs at startup", must report OK and exit 0 rather than "is not valid".
func TestConfigValidateAcceptsAnEmptyStub(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(tomlPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := configValidateCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("an empty stub self-heals at startup; validate must report OK, got: %v", err)
	}
	if !strings.Contains(out.String(), "config OK") {
		t.Errorf("validate must report OK for an empty stub, got: %q", out.String())
	}
	if !strings.Contains(out.String(), "empty stub") {
		t.Errorf("validate must name the empty-stub state distinctly from Missing, got: %q", out.String())
	}
}

// TestConfigValidateAcceptsAnEmptyStubDefaultHomeReadOnly is the chmod-repairable
// default-home case for this command: a contentless config.toml in an owner-owned
// default ~/.agent-factory tightened to a write-less mode (0500) is a state af
// self-heals at startup (secureAFHomeForPath chmod-repairs to 0700 before removing
// the stub). validate's stated contract is "the same parse+validate af runs at
// startup", so it must report OK and exit 0 — not the "cannot write to config
// directory: permission denied" the read-only diagnostic raised before the fix.
//
// Unlike TestConfigValidateAcceptsAnEmptyStub, this stages the home as the
// CONCRETE default via $HOME (AGENT_FACTORY_HOME empty) at a write-less mode, so
// secureAFHomeForPath's repair path — which the bug's gate wrongly ignored — is
// genuinely exercised.
func TestConfigValidateAcceptsAnEmptyStubDefaultHomeReadOnly(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses mode bits, so a 0500 home cannot be staged as non-writable")
	}
	t.Setenv("SHELL", "/bin/sh")
	userHome := t.TempDir()
	afHome := filepath.Join(userHome, ".agent-factory")
	if err := os.Mkdir(afHome, 0o755); err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(afHome, "config.toml")
	if err := os.WriteFile(tomlPath, []byte("# placeholder\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(afHome, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(afHome, 0o755) })
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", "")

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := configValidateCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("a chmod-repairable default home self-heals at startup; validate must report OK, got: %v", err)
	}
	if !strings.Contains(out.String(), "config OK") {
		t.Errorf("validate must report OK for a repairable default home, got: %q", out.String())
	}
	if !strings.Contains(out.String(), "empty stub") {
		t.Errorf("validate must name the empty-stub state, got: %q", out.String())
	}

	// No-write: the stub is untouched and the home stays read-only.
	after, _ := os.ReadFile(tomlPath)
	if string(after) != "# placeholder\n" {
		t.Errorf("validate changed the empty stub it checked.\n got: %q\nwant: %q", after, "# placeholder\n")
	}
	info, err := os.Stat(afHome)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Errorf("validate must not chmod-repair the home it reports on, got mode %o", info.Mode().Perm())
	}
}

// TestConfigValidateAcceptsACommentOnlyStub pins the #3196 widening: a comments
// -only config.toml (`# TODO fill this in`) is effectively empty, and startup
// self-heals it just like a zero-byte stub. validate must agree — not reject a
// comment placeholder af itself discards to defaults.
func TestConfigValidateAcceptsACommentOnlyStub(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(tomlPath, []byte("# TODO fill this in\n# another note\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := configValidateCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("a comments-only stub self-heals at startup; validate must report OK, got: %v", err)
	}
	if !strings.Contains(out.String(), "config OK") {
		t.Errorf("validate must report OK for a comments-only stub, got: %q", out.String())
	}
}

// TestConfigValidateDoesNotMutateAnEmptyStub extends the read-only promise to the
// empty-stub state: the command must neither remove the stub (startup does, but
// validate is a check, not a heal) nor materialize any config file beside it.
//
// Like TestConfigValidateDoesNotMutateTheConfig above, the assertion is scoped to
// CONFIG files rather than the whole home: every command runs log.Initialize,
// which writes agent-factory.log into the home — a known side effect, not a
// config mutation.
func TestConfigValidateDoesNotMutateAnEmptyStub(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, "config.toml")
	jsonPath := filepath.Join(home, "config.json")
	bakPath := filepath.Join(home, "config.json.bak")
	original := []byte("# placeholder\n")
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
		t.Errorf("validate changed the empty stub it checked.\n got: %q\nwant: %q", after, original)
	}
	if _, err := os.Stat(jsonPath); err == nil {
		t.Error("validate materialized a config.json — it must read, never write, even for an empty stub")
	}
	if _, err := os.Stat(bakPath); err == nil {
		t.Error("validate materialized a config.json.bak — it must read, never write, even for an empty stub")
	}
}

// TestConfigValidateJSONAcceptsAnEmptyStub holds the --json side of the contract:
// a script asking "will af load my config?" via the envelope must get OK:true for
// a state af boots on, not a structured error.
func TestConfigValidateJSONAcceptsAnEmptyStub(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(tomlPath, []byte("   \n"), 0644); err != nil {
		t.Fatal(err)
	}

	prev := configJSONFlag
	configJSONFlag = true
	defer func() { configJSONFlag = prev }()

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := configValidateCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("an empty stub must be OK under --json too, got: %v", err)
	}

	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not the {data,error} envelope: %v\ngot: %q", err, out.String())
	}
	if env.Error != nil {
		t.Fatalf("an empty stub must not be a structured error: %q", out.String())
	}
	var res configValidateResult
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatalf("envelope data is not a configValidateResult: %v (%q)", err, out.String())
	}
	if !res.OK {
		t.Errorf("an empty stub must report OK:true under --json, got ok=false (%q)", out.String())
	}
	if !strings.Contains(res.Path, "config.toml") {
		t.Errorf("an empty stub must report its toml path, got %q", res.Path)
	}
}

func TestConfigValidateJSONReportsAdvisoryUncertainty(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	previousLoad := configValidateLoadReadOnly
	configValidateLoadReadOnly = func() (config.ReadOnlyConfigLoad, error) {
		return config.ReadOnlyConfigLoad{
			Path:                   "/tmp/config.toml",
			EmptyStub:              true,
			DirectoryAccessWarning: "directory access probe unavailable: function not implemented",
		}, nil
	}
	t.Cleanup(func() { configValidateLoadReadOnly = previousLoad })
	previousJSON := configJSONFlag
	configJSONFlag = true
	t.Cleanup(func() { configJSONFlag = previousJSON })

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := configValidateCmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}

	var envelope struct {
		Data struct {
			OK        bool `json:"ok"`
			Uncertain bool `json:"uncertain"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, out.String())
	}
	if !envelope.Data.OK || !envelope.Data.Uncertain {
		t.Fatalf("advisory result = ok:%t uncertain:%t; want true/true\n%s",
			envelope.Data.OK, envelope.Data.Uncertain, out.String())
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, out.Bytes()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(compact.String(), `"warning":"directory access probe unavailable: function not implemented","uncertain":true`) {
		t.Fatalf("uncertain must be appended after the established result members:\n%s", compact.String())
	}
}

func TestConfigValidateHelpDescribesAdvisorySuccess(t *testing.T) {
	help := strings.Join(strings.Fields(configValidateCmd.Long), " ")
	for _, want := range []string{
		"exit 0 means no config defect was found",
		"does not prove that a later startup can regenerate an empty stub",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("validate help does not contain %q:\n%s", want, configValidateCmd.Long)
		}
	}
}
