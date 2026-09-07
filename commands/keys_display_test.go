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
						if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
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
