package commands

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

func captureProcessStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func resetCobraSilence(cmd *cobra.Command) {
	cmd.SilenceUsage = false
	cmd.SilenceErrors = false
	for _, child := range cmd.Commands() {
		resetCobraSilence(child)
	}
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

// configEntriesInternalKeys are the toml-tagged config.Config fields that are
// deliberately NOT readable through `af config get/list`. Every entry needs a
// reason: this is the only escape hatch from the reflective coverage check, so
// an unexplained addition is how the drift starts again.
var configEntriesInternalKeys = map[string]string{
	// Written and read by the config loader's migration machinery, not a user
	// setting: there is nothing for a user to do with the number, and printing
	// it in `af config list` would invite hand-editing a field that must only
	// ever be moved by a migration.
	"schema_version": "internal migration bookkeeping, not a user-settable setting",
}

// TestConfigEntriesCoverAllKeys guards that globalConfigReadOrder covers every
// toml-tagged field of config.Config, so a key added to the struct cannot ship
// unreadable through `af config get/list`.
//
// It REFLECTS over config.Config rather than comparing against a hand-written
// list. The previous hand-written version claimed this same guarantee in its
// docstring but never consulted the struct at all — it compared two hardcoded
// lists to each other, so adding a field could never fail it. That tautology
// silently permitted a 6-key drift (listen_addr, require_loopback_token,
// cors_allowed_origins, limit_auto_resume, limit_retry_interval and, later,
// vscode_server_binary were all documented-but-unreadable). Reflection is the
// point of this test: keep it, and add genuinely-internal keys to
// configEntriesInternalKeys WITH a reason rather than skipping the check.
//
// Only top-level fields are considered, which is the right granularity —
// globalConfigReadOrder is a top-level key list, and nested tables (root-agent
// profiles and theme slots) are surfaced as whole composite
// values by their parent key.
func TestConfigEntriesCoverAllKeys(t *testing.T) {
	got := map[string]bool{}
	for _, key := range globalConfigReadOrder {
		if got[key] {
			t.Fatalf("duplicate config key %q in globalConfigReadOrder", key)
		}
		got[key] = true
	}

	want := map[string]bool{}
	rt := reflect.TypeOf(config.Config{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		key, _, _ := strings.Cut(tag, ",")
		if key == "" || key == "-" {
			continue
		}
		if reason, internal := configEntriesInternalKeys[key]; internal {
			if got[key] {
				t.Errorf("config key %q is listed in globalConfigReadOrder but also marked internal (%s) — pick one", key, reason)
			}
			continue
		}
		want[key] = true
		if !got[key] {
			t.Errorf("config.Config field %s (toml:%q) is missing from globalConfigReadOrder: `af config get %s` "+
				"would fail even though the key configures a real setting. Add %q to globalConfigReadOrder "+
				"(and document it in docs/configuration.md), or add it to configEntriesInternalKeys with a reason.",
				f.Name, key, key, key)
		}
	}
	if len(want) == 0 {
		t.Fatal("reflection found no toml-tagged config keys — the test is not exercising config.Config")
	}

	for key := range got {
		if !want[key] {
			t.Errorf("globalConfigReadOrder lists key %q, which is not a toml-tagged field of config.Config "+
				"(stale entry or typo?)", key)
		}
	}
}

// TestConfigGetReadsEveryEntry drives `af config get` for every key
// globalConfigReadOrder advertises, so coverage means the key actually READS rather
// than merely appearing in the list — the value must render without error and
// print something for a key with a non-zero default. `af config list` must
// agree with each one.
func TestConfigGetReadsEveryEntry(t *testing.T) {
	tempAFHome(t)
	entries, err := loadGlobalConfigEntries()
	if err != nil {
		t.Fatalf("load config entries: %v", err)
	}

	var listOut bytes.Buffer
	listCmd := &cobra.Command{}
	listCmd.SetOut(&listOut)
	if err := configListCmd.RunE(listCmd, nil); err != nil {
		t.Fatalf("config list: %v", err)
	}
	list := listOut.String()

	for _, e := range entries {
		t.Run(e.Key, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			if err := configGetCmd.RunE(cmd, []string{e.Key}); err != nil {
				t.Fatalf("config get %s: %v", e.Key, err)
			}
			got := strings.TrimSpace(out.String())
			if !strings.Contains(list, e.Key) {
				t.Errorf("config list does not mention key %q", e.Key)
			}
			// A key whose default is a non-zero scalar must print that value
			// verbatim: this is what catches a wrong-typed entry (e.g. a bool
			// rendered through the JSON fallback as "true" is fine, but a
			// duration or address silently coming back empty is not).
			switch want := e.Value.(type) {
			case string:
				if want != "" && got != want {
					t.Errorf("config get %s = %q, want %q", e.Key, got, want)
				}
			case bool:
				if got != strconv.FormatBool(want) {
					t.Errorf("config get %s = %q, want %q", e.Key, got, strconv.FormatBool(want))
				}
			case int:
				if got != strconv.Itoa(want) {
					t.Errorf("config get %s = %q, want %q", e.Key, got, strconv.Itoa(want))
				}
			}
		})
	}
}

// TestConfigGetDocumentedGlobalOnlyKeys pins the specific keys that were
// documented in docs/configuration.md but unreadable through `af config get`
// until the reflective coverage test above forced them in. They are asserted
// by hand, with their real default values, because a caller following the docs
// runs exactly these commands.
func TestConfigGetDocumentedGlobalOnlyKeys(t *testing.T) {
	tempAFHome(t)
	cases := []struct{ key, want string }{
		{"listen_addr", "127.0.0.1:8443"},
		{"require_loopback_token", "false"},
		{"limit_auto_resume", "false"},
		{"limit_retry_interval", "30m"},
		{"vscode_server_binary", ""},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			if err := configGetCmd.RunE(cmd, []string{c.key}); err != nil {
				t.Fatalf("config get %s: %v", c.key, err)
			}
			if got := strings.TrimSpace(out.String()); got != c.want {
				t.Errorf("config get %s = %q, want %q", c.key, got, c.want)
			}
		})
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

func TestConfigGetUnknownKeyJSONRootSuppressesCobraAndLog(t *testing.T) {
	tempAFHome(t)
	prevJSON := configJSONFlag
	t.Cleanup(func() {
		configJSONFlag = prevJSON
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
		resetCobraSilence(rootCmd)
		if flag := configGetCmd.Flags().Lookup("json"); flag != nil {
			_ = flag.Value.Set("false")
			flag.Changed = false
		}
	})

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetArgs([]string{"config", "get", "not_a_key", "--json"})

	var execErr error
	stderr := captureProcessStderr(t, func() {
		rootCmd.SetErr(os.Stderr)
		execErr = rootCmd.Execute()
	})
	if execErr == nil {
		t.Fatal("expected root execute error for unknown key")
	}
	if stdout.Len() != 0 {
		t.Fatalf("error path must not write stdout, got: %s", stdout.String())
	}
	if strings.Contains(stderr, "Usage:") || strings.Contains(stderr, "Error:") ||
		strings.Contains(stderr, "wrote logs to") {
		t.Fatalf("--json stderr contains non-envelope text: %q", stderr)
	}

	var env struct {
		Data  any `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &env); err != nil {
		t.Fatalf("stderr is not a clean JSON envelope: %v\n%s", err, stderr)
	}
	if env.Data != nil {
		t.Errorf("failure envelope data should be null, got %v", env.Data)
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "unknown config key") {
		t.Fatalf("envelope missing the unknown-key message: %s", stderr)
	}
}

// TestConfigSetWritesAndReflects drives the set command through cobra and
// confirms the value is written and read back, comments in the seeded config
// preserved.
func TestConfigSetWritesAndReflects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("# hi\ndefault_program = 'claude'  # note\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := configSetCmd.RunE(cmd, []string{"default_program", "codex"}); err != nil {
		t.Fatalf("config set: %v", err)
	}

	got, _ := os.ReadFile(path)
	want := "# hi\ndefault_program = 'codex'  # note\nschema_version = 1\n"
	if string(got) != want {
		t.Fatalf("file not preserved.\n got: %q\nwant: %q", got, want)
	}
}

// TestConfigSetEchoesKeyValueAndRoundTrips is the end-to-end contract the config
// agent's briefing is written against, driven through the cobra commands against
// a throwaway AGENT_FACTORY_HOME.
//
// Three things are pinned, and each one is load-bearing for the walkthrough:
//
//  1. The echo line contains `key = value`. The briefing tells the agent to echo
//     exactly that after every set so the user can see what changed; the CLI
//     already prints it, which makes the shape a contract rather than a detail.
//  2. `af config set` prints the restart note itself. The briefing tells the
//     agent NOT to repeat it — an instruction that only makes sense while the CLI
//     still prints it, so if this line ever disappears the briefing silently
//     becomes wrong and the user stops being told to restart at all.
//  3. The value round-trips through `af config get`, the file still loads, and
//     the atomic write leaves no partial or temp file behind.
func TestConfigSetEchoesKeyValueAndRoundTrips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	path := filepath.Join(home, "config.toml")

	setCmd := &cobra.Command{}
	var setOut bytes.Buffer
	setCmd.SetOut(&setOut)
	if err := configSetCmd.RunE(setCmd, []string{"daemon_poll_interval", "2500"}); err != nil {
		t.Fatalf("config set: %v", err)
	}

	echo := setOut.String()
	if !strings.Contains(echo, "daemon_poll_interval = 2500") {
		t.Errorf("`af config set` must echo the change as `key = value` (the shape the config agent mirrors), got: %q", echo)
	}
	if !strings.Contains(echo, "restart") {
		t.Errorf("`af config set` must print the restart note itself — the config agent's briefing tells the "+
			"agent not to repeat it, so dropping it here means nobody tells the user. Got: %q", echo)
	}

	// Round-trip: the value comes back through the read side.
	getCmd := &cobra.Command{}
	var getOut bytes.Buffer
	getCmd.SetOut(&getOut)
	if err := configGetCmd.RunE(getCmd, []string{"daemon_poll_interval"}); err != nil {
		t.Fatalf("config get: %v", err)
	}
	if got := strings.TrimSpace(getOut.String()); got != "2500" {
		t.Errorf("`af config get daemon_poll_interval` = %q, want %q", got, "2500")
	}

	// The written file is complete and still loads — a partial write would show
	// up here rather than at the user's next startup.
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("config.toml unreadable after set: %v", err)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("config written by set does not load: %v", err)
	}
	if cfg.DaemonPollInterval != 2500 {
		t.Errorf("loaded daemon_poll_interval = %d, want 2500", cfg.DaemonPollInterval)
	}

	// The atomic write must not strand its temp file next to the real one.
	// AtomicWriteFile stages through "<name>.tmp.*" (config/filelock.go) and
	// renames; a surviving stage file means the rename never happened. The
	// config lock file and the relocated application log are expected
	// neighbours in an AGENT_FACTORY_HOME, so only the stage pattern is checked.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("atomic write left the staging file %q behind — the rename did not complete", e.Name())
		}
	}
}

// TestConfigSetBadValueJSONEnvelope pins that a failed `config set --json`
// emits the shared failure envelope on stderr, consistent with config get.
func TestConfigSetBadValueJSONEnvelope(t *testing.T) {
	tempAFHome(t)
	prev := configJSONFlag
	configJSONFlag = true
	defer func() { configJSONFlag = prev }()

	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := configSetCmd.RunE(cmd, []string{"default_program", "notanagent"})
	if err == nil {
		t.Fatal("expected error for invalid value")
	}
	if stdout.Len() != 0 {
		t.Fatalf("error path must not write stdout: %s", stdout.String())
	}
	var env struct {
		Data  any `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &env); err != nil {
		t.Fatalf("stderr not an envelope: %v\n%s", err, stderr.String())
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "must be one of") {
		t.Fatalf("envelope missing validation message: %s", stderr.String())
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
