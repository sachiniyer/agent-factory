package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellQuotePathQuotesEveryNonEmptyPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"empty", "", ""},
		{"plain path", "/usr/local/bin/claude", "'/usr/local/bin/claude'"},
		{"space", "/Applications/Claude Code.app/claude", "'/Applications/Claude Code.app/claude'"},
		{"metachar", "/tmp/R&D/bin/claude", "'/tmp/R&D/bin/claude'"},
		{"apostrophe", "/tmp/dev's tools/claude", "'/tmp/dev'\\''s tools/claude'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shellQuotePath(tt.path))
		})
	}
}

func TestDefaultConfigQuotesDetectedClaudePathWithShellMetacharacters(t *testing.T) {
	tests := []struct {
		name    string
		dirName string
	}{
		{"metacharacters without spaces", "R&D"},
		{"spaces and metacharacters", "Claude Path (R&D) $HOME"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := filepath.Join(t.TempDir(), tt.dirName)
			require.NoError(t, os.MkdirAll(binDir, 0755))
			target := filepath.Join(binDir, "claude")
			require.NoError(t, os.WriteFile(target, []byte("#!/bin/sh\nprintf 'argv:%s\\n' \"$*\"\n"), 0755))

			t.Setenv("HOME", t.TempDir())
			t.Setenv("SHELL", requireBash(t))
			t.Setenv("PATH", binDir)

			cfg := DefaultConfig()
			override := cfg.ProgramOverrides[tmux.ProgramClaude]
			require.Equal(t, shellSingleQuote(target)+" --dangerously-skip-permissions", override)

			out, err := exec.Command("/bin/sh", "-c", override).CombinedOutput()
			require.NoError(t, err, string(out))
			assert.Equal(t, "argv:--dangerously-skip-permissions", strings.TrimSpace(string(out)))
		})
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
