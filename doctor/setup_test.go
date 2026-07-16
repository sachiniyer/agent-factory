package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestSetupDoctorHealthy(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	repo := setupDoctorRepo(t)
	fakeTmux := writeExecutable(t, binDir, "tmux", "#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then echo 'tmux 3.4'; exit 0; fi\nexit 0\n")
	fakeAgent := writeExecutable(t, binDir, "fake-agent", "#!/bin/sh\nexit 0\n")
	require.NotEmpty(t, fakeTmux)

	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeSetupConfig(t, home, fakeAgent)
	chdir(t, repo)

	report, err := Run(Options{
		Setup:     true,
		ConfigDir: home,
		TempDir:   t.TempDir(),
		remoteConfig: func() (*config.RemoteHooks, string, error) {
			return nil, "", nil
		},
	})
	require.NoError(t, err)
	require.Zero(t, report.UnresolvedCount(), "findings: %+v", report.Findings)
	require.True(t, okContains(report, "default_program"))
	require.True(t, okContains(report, "git identity"))
	require.True(t, okContains(report, "log storage"))
}

func TestSetupDoctorReportsMissingDefaultProgram(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	writeExecutable(t, binDir, "tmux", "#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then echo 'tmux 3.4'; exit 0; fi\nexit 0\n")

	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("PATH", binDir)
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\n\n[program_overrides]\nclaude = \"/missing/claude\"\n"), 0o644))

	report, err := Run(Options{
		Setup:     true,
		ConfigDir: home,
		TempDir:   t.TempDir(),
		remoteConfig: func() (*config.RemoteHooks, string, error) {
			return nil, "", nil
		},
	})
	require.NoError(t, err)
	findings := findByCheck(report, "agent-program")
	require.NotEmpty(t, findings)
	require.Contains(t, findings[0].Detail, "default_program")
	require.Contains(t, findings[0].Detail, "program_overrides.claude")
}

func TestDoctorAgentBinaryScanIncludesAmp(t *testing.T) {
	binDir := t.TempDir()
	fakeAmp := writeExecutable(t, binDir, "amp", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramAmp,
		ProgramOverrides: map[string]string{tmux.ProgramAmp: fakeAmp},
	}
	report := &Report{}
	checkAgentBinaries(cfg, report)

	var agentsHeader string
	for _, h := range report.Header {
		if h.Label == "agents" {
			agentsHeader = h.Value
			break
		}
	}
	require.Contains(t, agentsHeader, "amp=present")
	require.True(t, okContains(report, "amp"), "amp pass row should be recorded")
}

func setupDoctorRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	require.NoError(t, exec.Command("git", "init", repo).Run())
	require.NoError(t, exec.Command("git", "-C", repo, "config", "user.email", "test@example.com").Run())
	require.NoError(t, exec.Command("git", "-C", repo, "config", "user.name", "Test User").Run())
	require.NoError(t, exec.Command("git", "-C", repo, "commit", "--allow-empty", "-m", "init").Run())
	return repo
}

func writeSetupConfig(t *testing.T, home, agentPath string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(home, 0o755))
	content := fmt.Sprintf("default_program = %q\n\n[program_overrides]\n%s = %q\n",
		tmux.ProgramClaude, tmux.ProgramClaude, agentPath)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName), []byte(content), 0o644))
}

func writeExecutable(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
	return path
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(old) })
}
