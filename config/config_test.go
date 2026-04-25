package config

import (
	"github.com/sachiniyer/agent-factory/log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain runs before all tests to set up the test environment
func TestMain(m *testing.M) {
	// Initialize the logger before any tests run
	log.Initialize(false)
	defer log.Close()

	exitCode := m.Run()
	os.Exit(exitCode)
}

func TestGetClaudeCommand(t *testing.T) {
	originalShell := os.Getenv("SHELL")
	originalPath := os.Getenv("PATH")
	defer func() {
		os.Setenv("SHELL", originalShell)
		os.Setenv("PATH", originalPath)
	}()

	t.Run("finds claude in PATH", func(t *testing.T) {
		// Create a temporary directory with a mock claude executable
		tempDir := t.TempDir()
		claudePath := filepath.Join(tempDir, "claude")

		// Create a mock executable
		err := os.WriteFile(claudePath, []byte("#!/bin/bash\necho 'mock claude'"), 0755)
		require.NoError(t, err)

		// Set PATH to include our temp directory
		os.Setenv("PATH", tempDir+":"+originalPath)
		os.Setenv("SHELL", "/bin/bash")

		result, err := GetClaudeCommand()

		assert.NoError(t, err)
		assert.True(t, strings.Contains(result, "claude"))
	})

	t.Run("handles missing claude command", func(t *testing.T) {
		// Set PATH to a directory that doesn't contain claude
		tempDir := t.TempDir()
		os.Setenv("PATH", tempDir)
		os.Setenv("SHELL", "/bin/bash")

		result, err := GetClaudeCommand()

		assert.Error(t, err)
		assert.Equal(t, "", result)
		assert.Contains(t, err.Error(), "claude command not found")
	})

	t.Run("handles empty SHELL environment", func(t *testing.T) {
		// Create a temporary directory with a mock claude executable
		tempDir := t.TempDir()
		claudePath := filepath.Join(tempDir, "claude")

		// Create a mock executable
		err := os.WriteFile(claudePath, []byte("#!/bin/bash\necho 'mock claude'"), 0755)
		require.NoError(t, err)

		// Set PATH and unset SHELL
		os.Setenv("PATH", tempDir+":"+originalPath)
		os.Unsetenv("SHELL")

		result, err := GetClaudeCommand()

		assert.NoError(t, err)
		assert.True(t, strings.Contains(result, "claude"))
	})

	t.Run("handles alias parsing", func(t *testing.T) {
		// Test core alias formats. Keep this regex in sync with the one used in
		// GetClaudeCommand so the test exercises the real extraction logic.
		aliasRegex := regexp.MustCompile(`(?:aliased to|->|=)\s*(.+)`)

		extract := func(output string) (string, bool) {
			matches := aliasRegex.FindStringSubmatch(output)
			if len(matches) < 2 {
				return "", false
			}
			return strings.TrimSpace(matches[1]), true
		}

		// Standard alias format
		got, ok := extract("claude: aliased to /usr/local/bin/claude")
		assert.True(t, ok)
		assert.Equal(t, "/usr/local/bin/claude", got)

		// Alias path containing spaces (e.g. macOS app bundle) must be preserved.
		got, ok = extract("claude: aliased to /Applications/Claude Code.app/Contents/MacOS/claude")
		assert.True(t, ok)
		assert.Equal(t, "/Applications/Claude Code.app/Contents/MacOS/claude", got)

		// Arrow format with spaces in the path.
		got, ok = extract("claude -> /path/with spaces/claude")
		assert.True(t, ok)
		assert.Equal(t, "/path/with spaces/claude", got)

		// Equals format with trailing whitespace should be trimmed.
		got, ok = extract("claude=/path/with spaces/claude   ")
		assert.True(t, ok)
		assert.Equal(t, "/path/with spaces/claude", got)

		// Direct path (no alias)
		_, ok = extract("/usr/local/bin/claude")
		assert.False(t, ok)
	})
}

func TestDefaultConfig(t *testing.T) {
	t.Run("creates config with default values", func(t *testing.T) {
		config := DefaultConfig()

		assert.NotNil(t, config)
		assert.NotEmpty(t, config.DefaultProgram)
		assert.False(t, config.AutoYes)
		assert.Equal(t, 1000, config.DaemonPollInterval)
		assert.NotEmpty(t, config.BranchPrefix)
		assert.True(t, strings.HasSuffix(config.BranchPrefix, "/"))
	})

}

func TestGetConfigDir(t *testing.T) {
	t.Run("returns valid config directory", func(t *testing.T) {
		configDir, err := GetConfigDir()

		assert.NoError(t, err)
		assert.NotEmpty(t, configDir)
		assert.True(t, strings.HasSuffix(configDir, ".agent-factory"))

		// Verify it's an absolute path
		assert.True(t, filepath.IsAbs(configDir))
	})

	t.Run("uses AGENT_FACTORY_HOME when set", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		customDir := t.TempDir()
		os.Setenv("AGENT_FACTORY_HOME", customDir)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, customDir, configDir)
	})

	t.Run("expands tilde in AGENT_FACTORY_HOME", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "~/.my-custom-config")

		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, filepath.Join(homeDir, ".my-custom-config"), configDir)
	})

	t.Run("falls back to default when AGENT_FACTORY_HOME is empty", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "")

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.True(t, strings.HasSuffix(configDir, ".agent-factory"))
	})

	t.Run("returns home dir when AGENT_FACTORY_HOME is exactly ~", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "~")

		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, homeDir, configDir)
	})

	t.Run("expands ~/foo correctly", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "~/foo")

		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, filepath.Join(homeDir, "foo"), configDir)
	})

	t.Run("returns error for malformed ~.config", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "~.config")

		configDir, err := GetConfigDir()
		assert.Error(t, err)
		assert.Empty(t, configDir)
		assert.Contains(t, err.Error(), "invalid tilde format")
	})

	t.Run("returns error for malformed ~config", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "~config")

		configDir, err := GetConfigDir()
		assert.Error(t, err)
		assert.Empty(t, configDir)
		assert.Contains(t, err.Error(), "invalid tilde format")
	})
}

func TestLoadConfig(t *testing.T) {
	t.Run("returns default config when file doesn't exist", func(t *testing.T) {
		// Use a temporary home directory to avoid interfering with real config
		originalHome := os.Getenv("HOME")
		tempHome := t.TempDir()
		os.Setenv("HOME", tempHome)
		defer os.Setenv("HOME", originalHome)

		config := LoadConfig()

		assert.NotNil(t, config)
		assert.NotEmpty(t, config.DefaultProgram)
		assert.False(t, config.AutoYes)
		assert.Equal(t, 1000, config.DaemonPollInterval)
		assert.NotEmpty(t, config.BranchPrefix)
	})

	t.Run("loads valid config file", func(t *testing.T) {
		// Create a temporary config directory
		tempHome := t.TempDir()
		configDir := filepath.Join(tempHome, ".agent-factory")
		err := os.MkdirAll(configDir, 0755)
		require.NoError(t, err)

		// Create a test config file
		configPath := filepath.Join(configDir, ConfigFileName)
		configContent := `{
			"default_program": "test-claude",
			"auto_yes": true,
			"daemon_poll_interval": 2000,
			"branch_prefix": "test/"
		}`
		err = os.WriteFile(configPath, []byte(configContent), 0644)
		require.NoError(t, err)

		// Override HOME environment
		originalHome := os.Getenv("HOME")
		os.Setenv("HOME", tempHome)
		defer os.Setenv("HOME", originalHome)

		config := LoadConfig()

		assert.NotNil(t, config)
		assert.Equal(t, "test-claude", config.DefaultProgram)
		assert.True(t, config.AutoYes)
		assert.Equal(t, 2000, config.DaemonPollInterval)
		assert.Equal(t, "test/", config.BranchPrefix)
	})

	t.Run("returns default config on invalid JSON", func(t *testing.T) {
		// Create a temporary config directory
		tempHome := t.TempDir()
		configDir := filepath.Join(tempHome, ".agent-factory")
		err := os.MkdirAll(configDir, 0755)
		require.NoError(t, err)

		// Create an invalid config file
		configPath := filepath.Join(configDir, ConfigFileName)
		invalidContent := `{"invalid": json content}`
		err = os.WriteFile(configPath, []byte(invalidContent), 0644)
		require.NoError(t, err)

		// Override HOME environment
		originalHome := os.Getenv("HOME")
		os.Setenv("HOME", tempHome)
		defer os.Setenv("HOME", originalHome)

		config := LoadConfig()

		// Should return default config when JSON is invalid
		assert.NotNil(t, config)
		assert.NotEmpty(t, config.DefaultProgram)
		assert.False(t, config.AutoYes)                  // Default value
		assert.Equal(t, 1000, config.DaemonPollInterval) // Default value
	})
}

func TestSaveConfig(t *testing.T) {
	t.Run("saves config to file", func(t *testing.T) {
		// Create a temporary config directory
		tempHome := t.TempDir()

		// Override HOME environment
		originalHome := os.Getenv("HOME")
		os.Setenv("HOME", tempHome)
		defer os.Setenv("HOME", originalHome)

		// Create a test config
		testConfig := &Config{
			DefaultProgram:     "test-program",
			AutoYes:            true,
			DaemonPollInterval: 3000,
			BranchPrefix:       "test-branch/",
		}

		err := SaveConfig(testConfig)
		assert.NoError(t, err)

		// Verify the file was created
		configDir := filepath.Join(tempHome, ".agent-factory")
		configPath := filepath.Join(configDir, ConfigFileName)

		assert.FileExists(t, configPath)

		// Load and verify the content
		loadedConfig := LoadConfig()
		assert.Equal(t, testConfig.DefaultProgram, loadedConfig.DefaultProgram)
		assert.Equal(t, testConfig.AutoYes, loadedConfig.AutoYes)
		assert.Equal(t, testConfig.DaemonPollInterval, loadedConfig.DaemonPollInterval)
		assert.Equal(t, testConfig.BranchPrefix, loadedConfig.BranchPrefix)
	})
}
