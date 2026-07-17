package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func checkSetup(ctx *scanContext, report *Report) {
	checkAFHome(ctx, report)
	cfg := checkConfig(ctx, report)
	checkStateDirs(ctx, report)
	checkLogStorage(report)
	checkGit(report)
	checkTmux(report)
	if cfg != nil {
		checkAgentPrograms(cfg, report)
	}
	checkDaemonHealth(ctx, report, ctx.opts.daemonHealth())
	checkRemoteSetup(ctx, report)
}

func checkAFHome(ctx *scanContext, report *Report) {
	if ctx.opts.ConfigDir == "" {
		report.Findings = append(report.Findings, Finding{
			Check:  "af-home",
			Detail: "agent-factory home could not be resolved",
		})
		return
	}
	if err := writableDir(ctx.opts.ConfigDir); err != nil {
		report.Findings = append(report.Findings, Finding{
			Check: "af-home",
			Detail: fmt.Sprintf("agent-factory home %s is not writable — "+
				"fix directory permissions or set AGENT_FACTORY_HOME to a writable directory",
				ctx.opts.ConfigDir) + ": " + err.Error(),
		})
		return
	}
	report.Pass(sectionConfig, "agent-factory home", fmt.Sprintf("writable at %s", ctx.opts.ConfigDir))
}

func checkStateDirs(ctx *scanContext, report *Report) {
	for _, d := range []struct {
		check string
		label string
		dir   string
	}{
		{"state-dir", "state storage", filepath.Join(ctx.opts.ConfigDir, "instances")},
		{"repo-state-dir", "repo state storage", filepath.Join(ctx.opts.ConfigDir, "repos")},
	} {
		if err := writableDir(d.dir); err != nil {
			report.Findings = append(report.Findings, Finding{
				Check:  d.check,
				Detail: fmt.Sprintf("%s directory %s is not writable — fix directory permissions: %v", d.label, d.dir, err),
			})
		} else {
			report.Pass(sectionConfig, d.label, fmt.Sprintf("writable at %s", d.dir))
		}
	}
}

func checkLogStorage(report *Report) {
	path := aflog.LogFilePath()
	if path == "" {
		report.Findings = append(report.Findings, Finding{
			Check:  "log-storage",
			Detail: "could not resolve application log path — set AGENT_FACTORY_HOME to a writable directory and retry",
		})
		return
	}
	dir := filepath.Dir(path)
	if err := writableDir(dir); err != nil {
		report.Findings = append(report.Findings, Finding{
			Check:  "log-storage",
			Detail: fmt.Sprintf("log directory %s is not writable — fix directory permissions or set AGENT_FACTORY_HOME to a writable directory: %v", dir, err),
		})
		return
	}
	report.Pass(sectionConfig, "log storage", fmt.Sprintf("writable at %s", dir))
}

func checkConfig(_ *scanContext, report *Report) *config.Config {
	cfg, err := config.LoadConfig()
	if err != nil {
		report.Findings = append(report.Findings, Finding{
			Check:  "config",
			Detail: fmt.Sprintf("config is not valid — fix the reported file or delete it to regenerate defaults: %v", err),
		})
		return nil
	}
	path, err := config.GlobalConfigPath()
	if err != nil {
		report.Findings = append(report.Findings, Finding{
			Check:  "config",
			Detail: fmt.Sprintf("could not resolve config path: %v", err),
		})
		return cfg
	}
	if _, statErr := os.Stat(path); statErr != nil {
		report.Findings = append(report.Findings, Finding{
			Check:  "config",
			Detail: fmt.Sprintf("config loaded in memory but %s is not materialized on disk — run `%s` or fix AF home permissions: %v", path, shellsuggest.Command("af", "config", "set", "default_program", cfg.DefaultProgram), statErr),
		})
		return cfg
	}
	report.Pass(sectionConfig, "config",
		fmt.Sprintf("loaded %s (default_program = %q)", path, cfg.DefaultProgram))
	return cfg
}

func checkGit(report *Report) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		report.Findings = append(report.Findings, Finding{
			Check:  "git",
			Detail: "git is not installed or not on PATH — install git and retry",
		})
		return
	}
	report.Pass(sectionEnvironment, "git", fmt.Sprintf("available at %s", gitPath))

	cwd, err := os.Getwd()
	if err != nil {
		report.Findings = append(report.Findings, Finding{
			Check:  "git-repo",
			Detail: fmt.Sprintf("could not determine current directory: %v", err),
		})
		return
	}
	out, err := exec.Command(gitPath, "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		report.Findings = append(report.Findings, Finding{
			Check:  "git-repo",
			Detail: fmt.Sprintf("%s is not inside a git repository — run `af` from a repo root or pass --repo where supported", cwd),
		})
		return
	}
	report.Pass(sectionConfig, "git repo", strings.TrimSpace(string(out)))
	checkGitIdentity(gitPath, cwd, report)
}

func checkGitIdentity(gitPath, cwd string, report *Report) {
	missing := []string{}
	for _, key := range []string{"user.name", "user.email"} {
		out, err := exec.Command(gitPath, "-C", cwd, "config", "--get", key).Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		report.Findings = append(report.Findings, Finding{
			Check: "git-identity",
			Detail: fmt.Sprintf("git identity is incomplete (missing %s) — run: "+
				"git config --global user.name \"Your Name\" && git config --global user.email you@example.com",
				strings.Join(missing, " and ")),
		})
		return
	}
	report.Pass(sectionEnvironment, "git identity", "user.name and user.email are configured")
}

func checkTmux(report *Report) {
	version, err := preflight.CheckTmux()
	if err != nil {
		report.Findings = append(report.Findings, Finding{Check: "tmux", Detail: err.Error()})
		return
	}
	report.Pass(sectionEnvironment, "tmux", version)
}

func checkAgentPrograms(cfg *config.Config, report *Report) {
	if cfg.DefaultProgram == "" {
		report.Findings = append(report.Findings, Finding{
			Check:  "agent-program",
			Detail: fmt.Sprintf("default_program is empty — set it to one of %s in ~/.agent-factory/config.toml", tmux.SupportedProgramsString()),
		})
		return
	}
	checkOneProgram("default_program", cfg.DefaultProgram, config.ResolveProgram(cfg, cfg.DefaultProgram), report)
	for _, agent := range sortedKeys(cfg.ProgramOverrides) {
		command := strings.TrimSpace(cfg.ProgramOverrides[agent])
		if command == "" {
			continue
		}
		checkOneProgram("program_overrides."+agent, agent, command, report)
	}
}

func checkOneProgram(field, agent, command string, report *Report) {
	check, err := preflight.CheckCommand(command)
	if err != nil {
		report.Findings = append(report.Findings, Finding{
			Check:  "agent-program",
			Detail: fmt.Sprintf("%s resolves to %q but is not runnable — %v", field, command, preflight.ProgramError(agent, command, err)),
		})
		return
	}
	report.Pass(sectionEnvironment, field, fmt.Sprintf("%q uses %s", command, check.Path))
}

func writableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".af-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	closeErr := f.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
