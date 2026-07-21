package autoupdate

import (
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// EnvironmentVariable is the process-level auto-update override. The global
// config remains the persistent operator-facing switch.
const EnvironmentVariable = "AGENT_FACTORY_AUTO_UPDATE"

// Enabled applies the existing config and environment opt-out contract. A
// valid environment value overrides config; an invalid value is reported and
// leaves the config value in force.
func Enabled(cfg *config.Config) bool {
	enabled := true
	if cfg != nil {
		enabled = cfg.AutoUpdate
	}
	raw, ok := os.LookupEnv(EnvironmentVariable)
	if !ok {
		return enabled
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	case "0", "false", "f", "no", "n", "off":
		return false
	case "":
		return enabled
	default:
		log.WarningLog.Printf("auto-update: ignoring invalid %s=%q (expected true/false, 1/0, yes/no, on/off)", EnvironmentVariable, raw)
		return enabled
	}
}
