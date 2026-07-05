package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/spf13/cobra"
)

// tempAFHome points AGENT_FACTORY_HOME at a fresh temp dir so config reads
// materialize DefaultConfig there instead of touching the real home or a
// running daemon's config.
func tempAFHome(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
}

func TestFormatConfigValue(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string bare", "claude", "claude"},
		{"bool", true, "true"},
		{"int", 1000, "1000"},
		{"map as json", map[string]string{"claude": "x"}, `{"claude":"x"}`},
		{"nil map", map[string]string(nil), "null"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatConfigValue(c.in); got != c.want {
				t.Fatalf("formatConfigValue(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestConfigEntriesCoverAllKeys guards that configEntries lists exactly the
// documented settable top-level config keys — if a key is added to the Config
// struct, this fails until the entry (and docs) are updated.
func TestConfigEntriesCoverAllKeys(t *testing.T) {
	got := map[string]bool{}
	for _, e := range configEntries(config.DefaultConfig()) {
		if got[e.Key] {
			t.Fatalf("duplicate config key %q in configEntries", e.Key)
		}
		got[e.Key] = true
	}
	want := []string{
		"default_program", "program_overrides", "auto_yes", "daemon_poll_interval",
		"log_max_size_mb", "log_max_backups", "branch_prefix", "detach_keys",
		"update_channel", "root_agents", "limit_patterns", "keys",
	}
	if len(got) != len(want) {
		t.Fatalf("configEntries has %d keys, want %d", len(got), len(want))
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("configEntries missing key %q", k)
		}
	}
}

func TestConfigGetScalarBare(t *testing.T) {
	tempAFHome(t)
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := configGetCmd.RunE(cmd, []string{"default_program"}); err != nil {
		t.Fatalf("config get default_program: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "claude" {
		t.Fatalf("config get default_program = %q, want %q", got, "claude")
	}
}

func TestConfigGetUnknownKeyErrors(t *testing.T) {
	tempAFHome(t)
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	err := configGetCmd.RunE(cmd, []string{"not_a_key"})
	if err == nil {
		t.Fatal("config get not_a_key: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestConfigGetUnknownKeyJSONEnvelope pins the #1206 Greptile fix: a failed
// `config get --json` must emit the shared {data,error} envelope on stderr, not
// a bare Go error, so scripts parsing --json still get a structured error.
func TestConfigGetUnknownKeyJSONEnvelope(t *testing.T) {
	tempAFHome(t)
	prev := configJSONFlag
	configJSONFlag = true
	defer func() { configJSONFlag = prev }()

	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := configGetCmd.RunE(cmd, []string{"not_a_key"})
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if stdout.Len() != 0 {
		t.Fatalf("error path must not write stdout, got: %s", stdout.String())
	}

	var env struct {
		Data  any `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &env); err != nil {
		t.Fatalf("stderr is not the shared envelope: %v\n%s", err, stderr.String())
	}
	if env.Data != nil {
		t.Errorf("failure envelope data should be null, got %v", env.Data)
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "unknown config key") {
		t.Fatalf("envelope missing the unknown-key message: %s", stderr.String())
	}
}

func TestConfigListJSONEnvelope(t *testing.T) {
	tempAFHome(t)
	prev := configJSONFlag
	configJSONFlag = true
	defer func() { configJSONFlag = prev }()

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := configListCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("config list --json: %v", err)
	}

	var env struct {
		Data  []configEntry `json:"data"`
		Error any           `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("output is not the shared envelope: %v\n%s", err, out.String())
	}
	if env.Error != nil {
		t.Fatalf("envelope error should be null, got %v", env.Error)
	}
	var haveDefaultProgram bool
	for _, e := range env.Data {
		if e.Key == "default_program" {
			haveDefaultProgram = true
		}
	}
	if !haveDefaultProgram {
		t.Fatalf("config list --json missing default_program: %s", out.String())
	}
}
