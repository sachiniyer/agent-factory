package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/planusage"
)

// afExecutable, afLookPath, userHomeDir and managedSettingsFiles are vars so
// tests can pin them.
var (
	afExecutable = os.Executable
	afLookPath   = exec.LookPath
	userHomeDir  = os.UserHomeDir

	managedSettingsFiles = []string{
		"/etc/claude-code/managed-settings.json",
		"/Library/Application Support/ClaudeCode/managed-settings.json",
	}
	managedStatusLineOnce sync.Once

	// planUsageEnabled is the opt-in: [backlog] plan_usage in the GLOBAL config,
	// false unless the user turned it on. A var so tests can pin it.
	planUsageEnabled = defaultPlanUsageEnabled
)

func defaultPlanUsageEnabled() bool {
	cfg, err := config.LoadConfig()
	return err == nil && cfg != nil && cfg.Backlog.PlanUsage
}

const defaultTapRefresh = 60

var settingsFlag = regexp.MustCompile(`(^|[\s'"])--settings($|[\s='"])`)

// hasSettingsFlag reports whether a launch command already passes --settings.
// Claude Code keeps only the last one, so af must not add a second.
func hasSettingsFlag(command string) bool {
	return settingsFlag.MatchString(command)
}

// tapBinary names the af binary for the status line. os.Executable can be a
// versioned path a package manager removes on upgrade, so the af on PATH wins
// when it is the same file.
func tapBinary() (string, error) {
	exe, err := afExecutable()
	if err != nil || exe == "" {
		return "", err
	}
	onPath, err := afLookPath("af")
	if err != nil || !filepath.IsAbs(onPath) {
		return exe, nil
	}
	a, errA := os.Stat(onPath)
	b, errB := os.Stat(exe)
	if errA == nil && errB == nil && os.SameFile(a, b) {
		return onPath, nil
	}
	return exe, nil
}

// planTapSettings is the --settings value that makes Claude Code run
// "af internal plan-tap" as its status line. --settings has a higher
// precedence than the user's settings, so the tap runs their status line
// itself and keeps their refreshInterval and padding. configRoot is the
// account's Claude config dir, or "" for the ambient one. "" when af cannot
// name its own binary.
func planTapSettings(configRoot string) string {
	if !planUsageEnabled() {
		return ""
	}
	exe, err := tapBinary()
	if err != nil || exe == "" {
		log.WarningLog.Printf("plan usage: cannot locate the af binary for the status line tap: %v", err)
		return ""
	}
	warnManagedStatusLine()
	statusLine := map[string]any{
		"type":            "command",
		"command":         tapCommand(exe),
		"refreshInterval": defaultTapRefresh,
	}
	if user, ok := planusage.ReadStatusLine(filepath.Join(claudeUserConfigDir(configRoot), "settings.json")); ok {
		if len(user.RefreshInterval) > 0 {
			statusLine["refreshInterval"] = user.RefreshInterval
		}
		if len(user.Padding) > 0 {
			statusLine["padding"] = user.Padding
		}
	}
	data, err := json.Marshal(map[string]any{"statusLine": statusLine})
	if err != nil {
		return ""
	}
	return string(data)
}

func claudeUserConfigDir(configRoot string) string {
	if configRoot != "" {
		return configRoot
	}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	home, _ := userHomeDir()
	return filepath.Join(home, ".claude")
}

// warnManagedStatusLine logs once when managed settings define the status
// line: they outrank --settings, so the tap never runs and af gets no plan
// readings.
func warnManagedStatusLine() {
	managedStatusLineOnce.Do(func() {
		for _, f := range managedSettingsFiles {
			if _, ok := planusage.ReadStatusLine(f); ok {
				log.WarningLog.Printf("plan usage: %s sets statusLine, so af cannot read plan usage from Claude Code", f)
				return
			}
		}
	})
}

// tapCommand is the status line command. It pins the daemon's resolved af home:
// a Claude launch that overrides HOME would otherwise make the tap resolve a
// different default home, find plan usage disabled there (or record into the
// wrong home) (Codex P2 on #6299).
func tapCommand(exe string) string {
	cmd := shellQuote(exe) + " " + planusage.TapCommandArgs
	if home, err := config.GetConfigDir(); err == nil && home != "" {
		cmd = "AGENT_FACTORY_HOME=" + shellQuote(home) + " " + cmd
	}
	return cmd
}
