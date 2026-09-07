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
