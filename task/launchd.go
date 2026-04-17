//go:build darwin

package task

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// maxCalendarIntervals caps the cartesian product size to prevent
// memory explosion from complex cron expressions like "*/1 */1 * * *".
const maxCalendarIntervals = 512

func getLaunchAgentLabel(t Task) string {
	return "com.agent-factory.task-" + t.ID
}

func getLaunchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create LaunchAgents directory: %w", err)
	}
	return dir, nil
}

func InstallScheduler(t Task) error {
	label := getLaunchAgentLabel(t)

	dir, err := getLaunchAgentsDir()
	if err != nil {
		return err
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	calendarXML, err := cronToCalendarIntervalXML(t.CronExpr)
	if err != nil {
		return fmt.Errorf("failed to convert cron expression: %w", err)
	}

	pathEnv := os.Getenv("PATH")
	homeEnv := os.Getenv("HOME")
	shellEnv := os.Getenv("SHELL")
	termEnv := os.Getenv("TERM")
	if termEnv == "" {
		termEnv = "xterm-256color"
	}

	logDir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}
	logPath := filepath.Join(logDir, "task-"+t.ID+".log")

	// Escape all interpolated values for XML safety.
	esc := html.EscapeString
	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>task</string>
        <string>run</string>
        <string>%s</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s</string>
        <key>HOME</key>
        <string>%s</string>
        <key>SHELL</key>
        <string>%s</string>
        <key>TERM</key>
        <string>%s</string>
    </dict>
    <key>StartCalendarInterval</key>
%s
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, esc(label), esc(execPath), esc(t.ID), esc(t.ProjectPath),
		esc(pathEnv), esc(homeEnv), esc(shellEnv), esc(termEnv),
		calendarXML, esc(logPath), esc(logPath))

	plistPath := filepath.Join(dir, label+".plist")

	// Unload existing agent if present (ignore errors).
	unloadCmd := exec.Command("launchctl", "unload", plistPath)
	_ = unloadCmd.Run()

	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}

	loadCmd := exec.Command("launchctl", "load", plistPath)
	if out, err := loadCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to load launch agent: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

func RemoveScheduler(t Task) error {
	label := getLaunchAgentLabel(t)

	dir, err := getLaunchAgentsDir()
	if err != nil {
		return err
	}

	plistPath := filepath.Join(dir, label+".plist")

	// Unload the launch agent (ignore error if not loaded).
	unloadCmd := exec.Command("launchctl", "unload", plistPath)
	_ = unloadCmd.Run()

	// Remove plist file.
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist file: %w", err)
	}

	return nil
}

// cronToCalendarIntervalXML converts a 5-field cron expression to launchd
// StartCalendarInterval XML fragment.
func cronToCalendarIntervalXML(cronExpr string) (string, error) {
	if err := ValidateCronExpr(cronExpr); err != nil {
		return "", err
	}

	fields := strings.Fields(cronExpr)

	type fieldDef struct {
		key string
		min int
		max int
	}
	defs := []fieldDef{
		{"Minute", 0, 59},
		{"Hour", 0, 23},
		{"Day", 1, 31},
		{"Month", 1, 12},
		{"Weekday", 0, 7},
	}

	type expandedField struct {
		key  string
		vals []int // nil = wildcard
	}

	var expanded []expandedField
	for i, def := range defs {
		vals, err := expandCronField(fields[i], def.min, def.max)
		if err != nil {
			return "", fmt.Errorf("failed to expand %s field: %w", def.key, err)
		}
		// Normalize weekday 7 -> 0 (both mean Sunday in cron; launchd uses 0).
		if def.key == "Weekday" && vals != nil {
			vals = normalizeDOWValues(vals)
		}
		expanded = append(expanded, expandedField{key: def.key, vals: vals})
	}

	// When both day-of-month (DOM) and day-of-week (DOW) are non-wildcard,
	// standard cron uses OR semantics: run on matching DOM OR matching DOW.
	// We handle this by building two separate sets of combos and merging them.
	domIdx, dowIdx := 2, 4

	// Detect when DOM or DOW is syntactically restricted but semantically
	// covers all possible values (e.g., DOW=0-6 or DOM=1-31). Under cron OR
	// semantics, "X OR every-day" collapses to "every day", so both the DOM
	// and DOW restrictions become irrelevant and we emit a single
	// wildcard-day dict.
	dowCoversAll := expanded[dowIdx].vals != nil && len(expanded[dowIdx].vals) >= 7
	domCoversAll := expanded[domIdx].vals != nil && len(expanded[domIdx].vals) >= 31
	if dowCoversAll || domCoversAll {
		expanded[dowIdx].vals = nil
		expanded[domIdx].vals = nil
	}

	bothDOMandDOW := expanded[domIdx].vals != nil && expanded[dowIdx].vals != nil

	// buildCombos builds the cartesian product of the given expanded fields.
	type combo map[string]int
	buildCombos := func(fields []expandedField) ([]combo, error) {
		result := []combo{{}}
		for _, ef := range fields {
			if ef.vals == nil {
				continue // wildcard, omit from dict
			}
			var newResult []combo
			for _, existing := range result {
				for _, val := range ef.vals {
					c := make(combo)
					for k, v := range existing {
						c[k] = v
					}
					c[ef.key] = val
					newResult = append(newResult, c)
				}
			}
			if len(newResult) > maxCalendarIntervals {
				return nil, fmt.Errorf("cron expression %q expands to too many intervals (%d > %d)", cronExpr, len(newResult), maxCalendarIntervals)
			}
			result = newResult
		}
		return result, nil
	}

	var combos []combo
	if bothDOMandDOW {
		// OR semantics: generate DOM combos (without DOW) and DOW combos (without DOM).
		domFields := make([]expandedField, len(expanded))
		copy(domFields, expanded)
		domFields[dowIdx] = expandedField{key: "Weekday", vals: nil} // exclude DOW

		dowFields := make([]expandedField, len(expanded))
		copy(dowFields, expanded)
		dowFields[domIdx] = expandedField{key: "Day", vals: nil} // exclude DOM

		domCombos, err := buildCombos(domFields)
		if err != nil {
			return "", err
		}
		dowCombos, err := buildCombos(dowFields)
		if err != nil {
			return "", err
		}
		combos = append(domCombos, dowCombos...)
		if len(combos) > maxCalendarIntervals {
			return "", fmt.Errorf("cron expression %q expands to too many intervals (%d > %d)", cronExpr, len(combos), maxCalendarIntervals)
		}
	} else {
		var err error
		combos, err = buildCombos(expanded)
		if err != nil {
			return "", err
		}
	}

	if len(combos) == 0 || (len(combos) == 1 && len(combos[0]) == 0) {
		return "", fmt.Errorf("cron expression %q results in no specific schedule", cronExpr)
	}

	// Deterministic key order for output.
	keyOrder := []string{"Month", "Day", "Weekday", "Hour", "Minute"}

	var sb strings.Builder
	sb.WriteString("    <array>\n")
	for _, c := range combos {
		sb.WriteString("        <dict>\n")
		for _, key := range keyOrder {
			if val, ok := c[key]; ok {
				fmt.Fprintf(&sb, "            <key>%s</key>\n            <integer>%d</integer>\n", key, val)
			}
		}
		sb.WriteString("        </dict>\n")
	}
	sb.WriteString("    </array>")

	return sb.String(), nil
}

// normalizeDOWValues maps weekday value 7 to 0 (both mean Sunday)
// and deduplicates/sorts the result.
func normalizeDOWValues(vals []int) []int {
	seen := make(map[int]bool)
	var unique []int
	for _, v := range vals {
		if v == 7 {
			v = 0
		}
		if !seen[v] {
			seen[v] = true
			unique = append(unique, v)
		}
	}
	sort.Ints(unique)
	return unique
}
