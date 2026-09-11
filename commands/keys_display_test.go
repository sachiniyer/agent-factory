package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/keys"
)

func TestKeysDisplayConfigRoundTrip(t *testing.T) {
	for _, spelling := range []string{"alt+space", "alt+ctrl+space", "space", "ctrl+space", "shift+up"} {
		t.Run(spelling, func(t *testing.T) {
			tempAFHome(t)
			path := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "config.toml")
			writeConfig := func(value string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(fmt.Sprintf("[keys]\nnew = %q\n", value)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeConfig(spelling)
			for _, jsonOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("json=%v", jsonOutput), func(t *testing.T) {
					var out bytes.Buffer
					keysCmd.SetOut(&out)
					t.Cleanup(func() { keysCmd.SetOut(nil) })
					args := []string{}
					if jsonOutput {
						args = []string{"--json"}
					}
					if err := keysCmd.ParseFlags(args); err != nil {
						t.Fatal(err)
					}
					if flag := keysCmd.Flags().Lookup("json"); flag != nil {
						t.Cleanup(func() { _ = keysCmd.Flags().Set("json", "false") })
					}
					if err := keysCmd.RunE(keysCmd, nil); err != nil {
						t.Fatal(err)
					}
					var printed string
					if jsonOutput {
						var rows []struct {
							Action string   `json:"action"`
							Keys   []string `json:"keys"`
						}
						var envelope map[string]json.RawMessage
						if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
							t.Fatalf("af keys --json must return an envelope: %v", err)
						}
						if len(envelope) != 2 || string(envelope["error"]) != "null" {
							t.Fatalf("want {data: [...], error: null}, got %s", out.String())
						}
						if err := json.Unmarshal(envelope["data"], &rows); err != nil {
							t.Fatal(err)
						}
						for _, row := range rows {
							if row.Action == "new" && len(row.Keys) == 1 {
								printed = row.Keys[0]
							}
						}
					} else {
						for _, line := range strings.Split(out.String(), "\n") {
							fields := strings.Fields(line)
							if len(fields) > 1 && fields[0] == "new" {
								printed = fields[1]
							}
						}
					}
					if printed != spelling {
						t.Errorf("af keys printed %q, want pasteable %q", printed, spelling)
					}
					writeConfig(printed)
					cfg, err := config.LoadConfig()
					if err != nil {
						t.Fatalf("config -> af keys -> config failed: %v", err)
					}
					if err := keys.ValidateOverrides(cfg.KeymapOverrides()); err != nil {
						t.Fatal(err)
					}
					writeConfig(spelling)
				})
			}
		})
	}
}

type suppressedKeyJSON struct {
	Key     string   `json:"key"`
	TakenBy []string `json:"taken_by"`
}

type keysDisplayJSONRow struct {
	Action       string              `json:"action"`
	Description  string              `json:"description"`
	Keys         []string            `json:"keys"`
	Default      []string            `json:"default"`
	SuppressedBy []suppressedKeyJSON `json:"suppressed_by"`
}

func runKeysDisplay(t *testing.T, jsonOutput bool) string {
	t.Helper()
	var out bytes.Buffer
	keysCmd.SetOut(&out)
	defer keysCmd.SetOut(nil)
	if err := keysCmd.Flags().Set("json", fmt.Sprint(jsonOutput)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = keysCmd.Flags().Set("json", "false") }()
	if err := keysCmd.RunE(keysCmd, nil); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func decodeKeysDisplayJSON(t *testing.T, output string) []keysDisplayJSONRow {
	t.Helper()
	var envelope struct {
		Data  []keysDisplayJSONRow `json:"data"`
		Error json.RawMessage      `json:"error"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatalf("af keys --json must return an envelope: %v\n%s", err, output)
	}
	if string(envelope.Error) != "null" {
		t.Fatalf("af keys --json error = %s, want null", envelope.Error)
	}
	return envelope.Data
}

func normalizedKeysDisplayLine(output, firstField string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == firstField {
			return strings.Join(fields, " ")
		}
	}
	return ""
}

func TestKeysDisplayReportsFullySuppressedDefaultByOneOverride(t *testing.T) {
	tempAFHome(t)
	path := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "config.toml")
	if err := os.WriteFile(path, []byte(`[keys]
quit = "Q"
new = "c"
up = ["u", "ctrl+p"]
tasks = "ctrl+t"
`), 0600); err != nil {
		t.Fatal(err)
	}

	textOutput := runKeysDisplay(t, false)
	for action, want := range map[string]string{
		"limit_retry":    "limit_retry — (c taken by new)",
		"switch_project": "switch_project — (ctrl+p taken by up)",
	} {
		got := normalizedKeysDisplayLine(textOutput, action)
		if got != want {
			t.Errorf("af keys %s row = %q, want %q\n%s", action, got, want, textOutput)
		}
	}

	jsonOutput := runKeysDisplay(t, true)
	rows := make(map[string]keysDisplayJSONRow)
	for _, row := range decodeKeysDisplayJSON(t, jsonOutput) {
		rows[row.Action] = row
	}
	for action, want := range map[string]struct {
		key   string
		taker string
	}{
		"limit_retry":    {"c", "new"},
		"switch_project": {"ctrl+p", "up"},
	} {
		row := rows[action]
		if len(row.Keys) != 0 || len(row.SuppressedBy) != 1 || row.SuppressedBy[0].Key != want.key || len(row.SuppressedBy[0].TakenBy) != 1 || row.SuppressedBy[0].TakenBy[0] != want.taker {
			t.Errorf("af keys --json %s = keys %v, suppressed_by %v; want no keys and %s taken by %s", action, row.Keys, row.SuppressedBy, want.key, want.taker)
		}
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(jsonOutput)); err != nil {
		t.Fatal(err)
	}
	wantOrderedRow := `{"action":"limit_retry","description":"retry","keys":[],"default":["c"],"rebound":false,"suppressed_by":[{"key":"c","taken_by":["new"]}]}`
	if !strings.Contains(compact.String(), wantOrderedRow) {
		t.Fatalf("suppressed_by must be appended after every existing row member; want %s in:\n%s", wantOrderedRow, jsonOutput)
	}
}

func TestKeysDisplayReportsPartiallySuppressedDefault(t *testing.T) {
	tempAFHome(t)
	path := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "config.toml")
	if err := os.WriteFile(path, []byte("[keys]\nnew = \"k\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	textOutput := runKeysDisplay(t, false)
	if got, want := normalizedKeysDisplayLine(textOutput, "up"), "up up (k taken by new)"; got != want {
		t.Fatalf("partially suppressed up row = %q, want %q\n%s", got, want, textOutput)
	}
	for _, row := range decodeKeysDisplayJSON(t, runKeysDisplay(t, true)) {
		if row.Action != "up" {
			continue
		}
		if len(row.Keys) != 1 || row.Keys[0] != "up" || len(row.SuppressedBy) != 1 || row.SuppressedBy[0].Key != "k" || len(row.SuppressedBy[0].TakenBy) != 1 || row.SuppressedBy[0].TakenBy[0] != "new" {
			t.Fatalf("partially suppressed up JSON = keys %v, suppressed_by %v; want [up], [{k [new]}]", row.Keys, row.SuppressedBy)
		}
		return
	}
	t.Fatal("af keys --json omitted up")
}

func TestKeysDisplayReportsFullySuppressedDefaultBySeveralOverrides(t *testing.T) {
	tempAFHome(t)
	path := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "config.toml")
	if err := os.WriteFile(path, []byte("[keys]\nnew = \"k\"\nquit = \"up\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	textOutput := runKeysDisplay(t, false)
	if got, want := normalizedKeysDisplayLine(textOutput, "up"), "up — (up taken by quit; k taken by new)"; got != want {
		t.Fatalf("fully suppressed multi-key up row = %q, want %q\n%s", got, want, textOutput)
	}
	for _, row := range decodeKeysDisplayJSON(t, runKeysDisplay(t, true)) {
		if row.Action != "up" {
			continue
		}
		if len(row.Keys) != 0 || len(row.SuppressedBy) != 2 ||
			row.SuppressedBy[0].Key != "up" || len(row.SuppressedBy[0].TakenBy) != 1 || row.SuppressedBy[0].TakenBy[0] != "quit" ||
			row.SuppressedBy[1].Key != "k" || len(row.SuppressedBy[1].TakenBy) != 1 || row.SuppressedBy[1].TakenBy[0] != "new" {
			t.Fatalf("fully suppressed multi-key up JSON = keys %v, suppressed_by %v", row.Keys, row.SuppressedBy)
		}
		return
	}
	t.Fatal("af keys --json omitted up")
}

func TestKeysDisplayIdentifiesSuppressedFixedAction(t *testing.T) {
	tempAFHome(t)
	path := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "config.toml")
	if err := os.WriteFile(path, []byte("[keys]\nnew = \"g\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	textOutput := runKeysDisplay(t, false)
	want := "jump to tab (number or name) — (g taken by new)"
	var got string
	for _, line := range strings.Split(textOutput, "\n") {
		if strings.Contains(line, "jump to tab (number or name)") {
			got = strings.Join(strings.Fields(line), " ")
			break
		}
	}
	if got != want {
		t.Fatalf("suppressed fixed action row = %q, want %q\n%s", got, want, textOutput)
	}
	for _, row := range decodeKeysDisplayJSON(t, runKeysDisplay(t, true)) {
		if row.Description != "jump to tab (number or name)" {
			continue
		}
		if row.Action != "" || len(row.Keys) != 0 || len(row.SuppressedBy) != 1 || row.SuppressedBy[0].Key != "g" || len(row.SuppressedBy[0].TakenBy) != 1 || row.SuppressedBy[0].TakenBy[0] != "new" {
			t.Fatalf("suppressed fixed-action JSON = action %q, keys %v, suppressed_by %v", row.Action, row.Keys, row.SuppressedBy)
		}
		return
	}
	t.Fatal("af keys --json omitted fixed jump-to-tab action")
}

func TestKeysJSONConfigErrorEnvelope(t *testing.T) {
	tempAFHome(t)
	path := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "config.toml")
	if err := os.WriteFile(path, []byte("[keys]\nnew = \"alt+\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	keysCmd.SetOut(&out)
	keysCmd.SetErr(&errOut)
	root := keysCmd.Root()
	rootErrors, rootUsage := root.SilenceErrors, root.SilenceUsage
	cmdErrors, cmdUsage := keysCmd.SilenceErrors, keysCmd.SilenceUsage
	t.Cleanup(func() {
		keysCmd.SetOut(nil)
		keysCmd.SetErr(nil)
		_ = keysCmd.Flags().Set("json", "false")
		root.SilenceErrors, root.SilenceUsage = rootErrors, rootUsage
		keysCmd.SilenceErrors, keysCmd.SilenceUsage = cmdErrors, cmdUsage
	})
	if err := keysCmd.ParseFlags([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if err := keysCmd.RunE(keysCmd, nil); err == nil {
		t.Fatal("invalid config must fail")
	}
	if out.Len() != 0 {
		t.Fatalf("failure wrote stdout: %s", out.String())
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(errOut.Bytes(), &envelope); err != nil {
		t.Fatalf("stderr must contain the error envelope: %v; got %q", err, errOut.String())
	}
	if len(envelope) != 2 || string(envelope["data"]) != "null" {
		t.Fatalf("want {data: null, error: {...}}, got %s", errOut.String())
	}
	var failure struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(envelope["error"], &failure); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failure.Message, path) {
		t.Fatalf("config error must name %s: %s", path, failure.Message)
	}
}
