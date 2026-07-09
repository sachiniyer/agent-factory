package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func checkConfigAndStorage(ctx *scanContext, report *Report) *config.Config {
	load, cfgErr := config.LoadConfigReadOnly()
	cfg := load.Config
	channel := "unknown"
	if cfg != nil && cfg.UpdateChannel != "" {
		channel = cfg.UpdateChannel
	} else if load.Missing {
		channel = config.UpdateChannelStable
	}

	version := ctx.opts.Version
	if version == "" {
		version = "dev"
	}
	report.AddHeader("af", fmt.Sprintf("%s (channel: %s)", version, channel))
	report.AddHeader("os", runtime.GOOS+"/"+runtime.GOARCH)
	report.AddHeader("home", ctx.opts.ConfigDir)
	report.Pass(sectionEnvironment, "af", fmt.Sprintf("%s (channel: %s)", version, channel))
	report.Pass(sectionEnvironment, "os", runtime.GOOS+"/"+runtime.GOARCH)

	reportConfigValidity(report, load, cfgErr)
	checkHomeHealth(ctx, report)
	repoRoot, repoErr := currentRepoRoot()
	mode := worktreeMode(cfg, load.Missing)
	report.AddHeader("repo", repoHeader(repoRoot, repoErr, mode))
	checkWorktreeMode(ctx, report, repoRoot, repoErr, mode)
	return cfg
}

func checkEnvironment(_ *scanContext, report *Report, cfg *config.Config) {
	checkTmuxEnvironment(report)
	checkAgentBinaries(cfg, report)
}

func reportConfigValidity(report *Report, load config.ReadOnlyConfigLoad, err error) {
	switch {
	case err != nil:
		path := load.Path
		if path == "" {
			path = "global config"
		}
		report.Fail(sectionConfig, "config", fmt.Sprintf("%s is not valid: %v", path, err),
			"edit the config file, or delete it to regenerate defaults")
	case load.Missing:
		report.Warn(sectionConfig, "config", fmt.Sprintf("no config file at %s; defaults will be created on first write", load.Path),
			"run `af` once to materialize defaults or create config.toml", false)
	case load.LegacyJSON:
		report.Warn(sectionConfig, "config", fmt.Sprintf("loaded legacy config.json at %s", load.Path),
			"run `af` once to convert it to config.toml", false)
	case load.ShadowedJSON:
		report.Warn(sectionConfig, "config", fmt.Sprintf("loaded %s; config.json is also present and ignored", load.Path),
			"delete or rename the shadowed config.json", false)
	default:
		report.Pass(sectionConfig, "config",
			fmt.Sprintf("loaded %s (default_program: %s)", load.Path, load.Config.DefaultProgram))
	}
}

func checkHomeHealth(ctx *scanContext, report *Report) {
	home := ctx.opts.ConfigDir
	info, err := os.Stat(home)
	switch {
	case err == nil && info.IsDir():
		report.Pass(sectionConfig, "home", "readable directory at "+home)
	case err == nil:
		report.Fail(sectionConfig, "home", home+" exists but is not a directory",
			"move the file aside and create an agent-factory home directory")
	case os.IsNotExist(err):
		report.Warn(sectionConfig, "home", home+" does not exist yet",
			"run `af` once to create it, or create the directory manually", false)
	default:
		report.Fail(sectionConfig, "home", fmt.Sprintf("cannot stat %s: %v", home, err),
			"fix permissions or set AGENT_FACTORY_HOME to a readable directory")
	}

	for _, subdir := range []string{"instances", "repos"} {
		checkStorageDir(report, filepath.Join(home, subdir), subdir)
	}
}

func checkStorageDir(report *Report, path, name string) {
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		report.Pass(sectionConfig, name, "present at "+path)
	case err == nil:
		report.Fail(sectionConfig, name, path+" exists but is not a directory",
			"move the file aside so af can use this storage directory")
	case os.IsNotExist(err):
		report.Pass(sectionConfig, name, "not present; created on demand at "+path)
	default:
		report.Fail(sectionConfig, name, fmt.Sprintf("cannot stat %s: %v", path, err),
			"fix permissions on the agent-factory home")
	}
}

func currentRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	repo, err := config.RepoFromPath(cwd)
	if err != nil {
		return "", err
	}
	return repo.Root, nil
}

func worktreeMode(cfg *config.Config, missingConfig bool) string {
	if cfg != nil && cfg.WorktreeRoot != "" {
		return cfg.WorktreeRoot
	}
	if missingConfig {
		return config.WorktreeRootSibling
	}
	return "unknown"
}

func repoHeader(repoRoot string, repoErr error, mode string) string {
	if repoErr != nil {
		return "n/a (not inside a git repository)"
	}
	return fmt.Sprintf("%s (worktree_root: %s)", repoRoot, mode)
}

func checkWorktreeMode(ctx *scanContext, report *Report, repoRoot string, repoErr error, mode string) {
	if repoErr != nil {
		report.Warn(sectionConfig, "worktrees", "repo-scoped worktree checks skipped: "+repoErr.Error(),
			"run `af doctor` from inside a git repository", false)
		return
	}

	parent := ""
	switch mode {
	case config.WorktreeRootSubdirectory:
		parent = filepath.Join(ctx.opts.ConfigDir, "worktrees")
	case config.WorktreeRootSibling:
		parent = filepath.Dir(repoRoot)
	default:
		report.Warn(sectionConfig, "worktrees", "worktree_root is unknown because config did not load",
			"fix config first, then rerun `af doctor`", true)
		return
	}

	info, err := os.Stat(parent)
	switch {
	case err == nil && info.IsDir():
		report.Pass(sectionConfig, "worktrees", fmt.Sprintf("%s mode; parent %s is present", mode, parent))
	case err == nil:
		report.Fail(sectionConfig, "worktrees", fmt.Sprintf("%s mode parent %s exists but is not a directory", mode, parent),
			"move the file aside or change worktree_root")
	case os.IsNotExist(err):
		report.Warn(sectionConfig, "worktrees", fmt.Sprintf("%s mode parent %s does not exist yet", mode, parent),
			"create the directory or let af create it on the next session", false)
	default:
		report.Fail(sectionConfig, "worktrees", fmt.Sprintf("cannot stat %s mode parent %s: %v", mode, parent, err),
			"fix permissions or change worktree_root")
	}
}

func checkTmuxEnvironment(report *Report) {
	version, err := preflight.CheckTmux()
	if err != nil {
		report.AddHeader("tmux", "unavailable")
		report.Fail(sectionEnvironment, "tmux", "not available or not runnable", err.Error())
		return
	}
	report.AddHeader("tmux", version)
	report.Pass(sectionEnvironment, "tmux", version)
}

func checkAgentBinaries(cfg *config.Config, report *Report) {
	header := make([]string, 0, len(tmux.SupportedPrograms))
	for _, agent := range tmux.SupportedPrograms {
		command := agent
		configured := false
		if cfg != nil {
			command = config.ResolveProgram(cfg, agent)
			configured = cfg.DefaultProgram == agent || strings.TrimSpace(cfg.ProgramOverrides[agent]) != ""
		}
		check, err := preflight.CheckCommand(command)
		if err == nil {
			header = append(header, agent+"=present")
			report.Pass(sectionEnvironment, agent, fmt.Sprintf("%q resolves to %s", command, check.Path))
			continue
		}
		header = append(header, agent+"=missing")
		detail := fmt.Sprintf("%q is not runnable: %v", command, err)
		remediation := fmt.Sprintf("install %s or set program_overrides.%s", agent, agent)
		if configured {
			report.Fail(sectionEnvironment, agent, detail, remediation)
		} else {
			report.Warn(sectionEnvironment, agent, detail, remediation, false)
		}
	}
	report.AddHeader("agents", strings.Join(header, ", "))
}
