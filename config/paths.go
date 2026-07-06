package config

import (
	"os"
	"path/filepath"
	"strings"
)

// This file holds the config package's pure path/shell string helpers,
// extracted from config.go (#1146) to keep that file under its length ceiling
// (docs/file-length-lint.md). They are behavior-identical to their previous
// in-config.go definitions — same package, no call-site changes.

// prettyHomePath returns absPath with the user's home directory prefix
// collapsed to "~". Used to render config-file paths in user-facing errors
// without leaking the absolute filesystem layout. Returns absPath unchanged
// when the home directory cannot be determined or is not a prefix.
func prettyHomePath(absPath string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		return absPath
	}
	if absPath == homeDir {
		return "~"
	}
	if strings.HasPrefix(absPath, homeDir+string(filepath.Separator)) {
		return "~" + absPath[len(homeDir):]
	}
	return absPath
}

// shellQuotePath wraps a non-empty filesystem path in single quotes, escaping
// any embedded apostrophes with the standard POSIX single-quote escape idiom.
// Used by DefaultConfig when persisting auto-detected claude paths into
// ProgramOverrides — the value is passed to `sh -c` by tmux, so shell
// metacharacters in paths must never be left for the shell to interpret.
func shellQuotePath(path string) string {
	if path == "" {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
