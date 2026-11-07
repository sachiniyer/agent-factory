package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func checkConfigAndStorage(ctx *scanContext, report *Report) *config.Config {
	load, cfgErr := config.LoadConfigReadOnly()
	ctx.globalConfigMissing = load.Missing
	cfg := load.Config
	channel := "unknown"
	if cfg != nil && cfg.UpdateChannel != "" {
		channel = cfg.UpdateChannel
	} else if load.Missing || load.EmptyStub {
		// Both states self-heal to DefaultConfig on the next start, so the
		// channel af will actually run on is the default — not "unknown".
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
	mode := worktreeMode(cfg, load.Missing, load.EmptyStub)
	report.AddHeader("repo", repoHeader(repoRoot, repoErr, mode))
	checkWorktreeMode(ctx, report, repoRoot, repoErr, mode)
	return cfg
}

func checkEnvironment(ctx *scanContext, report *Report, cfg *config.Config) {
	checkTmuxEnvironment(report)
	checkTmuxServerState(ctx, report)
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
	case load.EmptyStub:
		// A contentless config.toml with no shadowing config.json is a state
		// af runs past on in-memory defaults (#4483 — a load never deletes),
		// so it is a WARN (advisory), not a FAIL: a health check that exits 1
		// for a state af considers healthy disagrees with the thing it
		// diagnoses. problem=false keeps it out of UnresolvedCount so
		// `af doctor` exits 0, matching startup.
		report.Warn(sectionConfig, "config", fmt.Sprintf("empty config stub at %s; af runs on built-in defaults and leaves the file untouched", load.Path),
			"write your settings to it, or delete it and run `af` to regenerate defaults", false)
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
	checkHookLogs(report, filepath.Join(home, "logs", "hooks"))
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

func currentRepoContext() (*config.RepoContext, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return config.RepoFromPath(cwd)
}

func currentRepoRoot() (string, error) {
	repo, err := currentRepoContext()
	if err != nil {
		return "", err
	}
	return repo.Root, nil
}

// resolveEffectiveConfig merges the in-repo config layer over the global cfg
// the way session launch does (config.ResolveConfigForRepoInspectionWithGlobalNonStrict),
// so the doctor verifies the same command a session's tmux pane will actually
// execute, not the global layer alone. It returns the effective config, the
// effective `backend` runtime selector (empty meaning local), the full
// resolved config (non-nil only when a repo layer was actually resolved, so
// callers can run session-creation's backend precondition checks), and any
// error from resolving the in-repo layer. Outside a repo (repo == nil) the
// global cfg is returned with a local backend and no error — that is the only
// layer sessions could launch under there. A nil global config returns
// (nil, local, nil, nil); callers must nil-check the config before using it.
//
// The non-strict project-registry path matches the runtime resolve
// (ResolveConfigForRepo): an unreadable personal/project-registry record
// degrades to "no personal layer" with a warning instead of failing, so one
// bad record does not make `af doctor` FAIL every repo session creation can
// still launch in.
func resolveEffectiveConfig(global *config.Config, repo *config.RepoContext) (*config.Config, string, *config.ResolvedConfig, error) {
	if global == nil {
		return nil, config.BackendLocal, nil, nil
	}
	if repo == nil {
		return global, config.BackendLocal, nil, nil
	}
	resolved, err := config.ResolveConfigForRepoInspectionWithGlobalNonStrict(repo, global)
	if err != nil {
		return nil, "", nil, err
	}
	return &resolved.Config, resolved.Backend, resolved, nil
}

// isLocalBackend reports whether a repo's resolved backend runs the agent on
// the daemon's own host — the only case where the daemon host's tmux and agent
// binary decide whether a create succeeds. docker/ssh/hook/sandbox run the
// agent elsewhere, so the daemon host's binaries are not prerequisites for
// them. Mirrors daemon.createNeedsDaemonHostPrereqs.
func isLocalBackend(backend string) bool {
	return backend == "" || backend == config.BackendLocal
}

func worktreeMode(cfg *config.Config, missingConfig, emptyStub bool) string {
	if cfg != nil && cfg.WorktreeRoot != "" {
		return cfg.WorktreeRoot
	}
	if missingConfig || emptyStub {
		// Both self-heal to DefaultConfig on the next start, whose
		// WorktreeRoot is WorktreeRootSibling — not the "unknown" the empty
		// (no-config-loadable) case is, which would emit a problem WARN and
		// make `af doctor` exit 1 for a state af boots cleanly on.
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
	// Resolve the effective config the way session launch does: when the doctor
	// is inside a git repo, merge the in-repo layer over the global config
	// (config.ResolveConfigForRepoInspectionWithGlobalNonStrict) so the command and
	// severity reflect what a session's tmux pane will actually execute, not
	// the global layer alone. Outside a repo the global config is the only
	// layer sessions could launch under, so it is kept unchanged.
	//
	// Anchor relative program_overrides to the session's launch directory (the
	// repo the doctor is inside), mirroring the daemon-side #5779 fix that
	// passes repo.WorkspacePath() to preflight.LocalSessionPrereqsAt. The doctor
	// used the cwd-fallback CheckCommand instead, so a relative override flipped
	// between PASS and FAIL purely on where `af doctor` was invoked. Outside any
	// repo the launch directory cannot be determined, so today's CheckCommand
	// is kept and any failure is reported with an honest message instead of
	// install/reinstall guidance (#6487).
	repo, repoErr := currentRepoContext()
	workspace := ""
	if repoErr == nil {
		workspace = repo.Root
	}
	effective, backend, resolved, resolveErr := resolveEffectiveConfig(cfg, repo)
	if resolveErr != nil {
		// A repo whose in-repo config exists but cannot be resolved refuses
		// session creation with this same error (#5801): the backend is
		// undeterminable, and guessing local could run the agent somewhere
		// other than the isolation the repo chose. Report it as an actionable
		// config failure instead of checking a command the session would
		// never launch. The resolver error itself names the offending layer
		// (checked-in, legacy, or personal-project), so the remediation stays
		// layer-neutral rather than unconditionally pointing at the checked-in
		// in-repo file — a malformed personal-project config reaches this
		// branch too, and "fix .agent-factory/config.toml" would not clear it.
		report.AddHeader("agents", "n/a (repo config unresolved)")
		report.Fail(sectionConfig, "agent-config",
			fmt.Sprintf("could not resolve config for the repo at %s: %v", repo.Root, resolveErr),
			"fix the config layer named in the error above and rerun `af doctor`")
		return
	}
	if !isLocalBackend(backend) {
		// Validate the selector the way session creation does (ParseBackendKind):
		// an unknown value such as `backend = "dokcer"` is a misconfiguration
		// session creation refuses, not a remote backend to skip host checks for.
		// Without this, both doctor profiles would skip the agent checks and exit
		// cleanly while claiming sessions run on a backend af itself rejects.
		kind, kerr := session.ParseBackendKind(backend)
		if kerr != nil {
			report.AddHeader("agents", fmt.Sprintf("n/a (unknown backend %q)", backend))
			report.Fail(sectionConfig, "agent-config",
				fmt.Sprintf("unknown backend %q in the in-repo config: %v", backend, kerr),
				"set `backend` to one of "+config.SupportedBackendsString()+" in the in-repo config (.agent-factory/config.toml) and rerun `af doctor`")
			return
		}
		// A supported non-local backend still has to be USABLE for a session in
		// this repo before the skip below is honest: config resolution accepts
		// `backend = "docker"` without docker.image, `backend = "ssh"` without
		// ssh.host, or `backend = "hook"` without remote_hooks, and a configured
		// backend can still be unusable — no docker/ssh CLI, no `origin` remote,
		// a remote Docker engine, or a hook command that cannot run. Run the
		// same side-effect-free preconditions session creation does
		// (BackendUnusableReason), so the doctor does not exit cleanly for a
		// repo whose every default create would fail (#6792 item 4).
		if resolved != nil {
			if reason := session.BackendUnusableReason(kind, resolved, repo.Root); reason != nil {
				report.AddHeader("agents", fmt.Sprintf("n/a (backend %q unusable)", backend))
				report.Fail(sectionConfig, "agent-config",
					fmt.Sprintf("backend %q is not usable for sessions in this repo: %v", backend, reason),
					"address the problem named above and rerun `af doctor`")
				return
			}
		}
		// docker/ssh/hook/sandbox run the agent off the daemon host, so the
		// host's agent binaries are not prerequisites for them — mirroring
		// daemon.createNeedsDaemonHostPrereqs, which skips LocalSessionPrereqs
		// for non-local backends.
		report.AddHeader("agents", fmt.Sprintf("skipped (repo backend: %s)", backend))
		report.Info(sectionEnvironment, "agents",
			fmt.Sprintf("host agent binaries not checked — this repo's sessions run on the %q backend", backend))
		return
	}
	header := make([]string, 0, len(tmux.SupportedPrograms))
	for _, agent := range tmux.SupportedPrograms {
		command := agent
		configured := false
		if effective != nil {
			command = config.ResolveProgram(effective, agent)
			configured = effective.DefaultProgram == agent || strings.TrimSpace(effective.ProgramOverrides[agent]) != ""
		}
		check, err, outsideRepo := checkProgramCommand(workspace, repoErr, command)
		if err == nil {
			// A resolution that depended on the launch directory and landed
			// inside the repo is honest only when the tree a session branches
			// from — origin's default branch, else HEAD — holds a runnable
			// file at the same repo-relative path: the worktree is a clean
			// checkout of that tree, not of this checkout's index or working
			// tree (#6499, #6792). Downgrade the PASS to an advisory WARN so
			// the doctor does not report a guaranteed-false PASS for a file
			// the session's worktree will not contain.
			if detail, remediation := overrideAbsentFromSessionBase(workspace, check); detail != "" {
				header = append(header, agent+"=present")
				report.Warn(sectionEnvironment, agent, detail, remediation, false)
				continue
			}
			header = append(header, agent+"=present")
			report.Pass(sectionEnvironment, agent, fmt.Sprintf("%q resolves to %s", command, check.Path))
			continue
		}
		header = append(header, agent+"=missing")
		detail := fmt.Sprintf("%q is not runnable: %v", command, err)
		remediation := preflight.ProgramError(agent, command, err).Error()
		if outsideRepo {
			remediation = noAnchorMessage(command)
		}
		if configured {
			report.Fail(sectionEnvironment, agent, detail, remediation)
		} else {
			report.Warn(sectionEnvironment, agent, detail, remediation, false)
		}
	}
	report.AddHeader("agents", strings.Join(header, ", "))
}

// checkTmuxServerState reports how the daemon is hosting its sessions (#6180) and
// what ended the last dedicated server (#5429). Both come from files the daemon
// writes, so a host that never ran the Linux daemon has neither and says nothing.
//
// The server-state record is the same read listTmuxSessions pins the session
// listing to, so the row and the listing share one memoized observation
// (recordedServer) — a divergent second read could disagree with the listing
// the orphaned-process check just armed a kill from.
func checkTmuxServerState(ctx *scanContext, report *Report) {
	if ctx.opts.ConfigDir == "" {
		return
	}
	state, ok, err := ctx.recordedServer()
	switch {
	case err != nil:
		report.Warn(sectionEnvironment, "tmux server", fmt.Sprintf("could not read the daemon's tmux server record: %v", err),
			"delete tmux-server-state.json in the AF home; the daemon rewrites it", false)
	case ok && state.Mode == tmux.ServerModeFallback:
		report.Warn(sectionEnvironment, "tmux server",
			fmt.Sprintf("the dedicated tmux server could not be launched (%s), so sessions run in per-session systemd scopes on the shared socket %s: any plain `tmux` on this machine reaches every session, `kill-server` included", state.Reason, state.Socket),
			"see tmux-server.log in the AF home; when the cause is gone the daemon launches a dedicated server again once no tmux server is running. For your own tmux use `env -u TMUX tmux -S <your own socket>`", true)
	case ok && state.Mode == tmux.ServerModeAdopted:
		report.Pass(sectionEnvironment, "tmux server",
			fmt.Sprintf("adopted an already-running tmux server on socket %s — af did not launch it and leaves its lifecycle alone", state.Socket))
	case ok:
		report.Pass(sectionEnvironment, "tmux server", fmt.Sprintf("dedicated, unit %s.scope, socket %s", state.Unit, state.Socket))
	}
	if ok && len(state.PriorSockets) > 0 {
		report.Info(sectionEnvironment, "tmux server history",
			fmt.Sprintf("earlier daemon generations recorded %d prior socket(s): %s — sessions may still be live on them, and doctor accounts for each before calling a session dead",
				len(state.PriorSockets), strings.Join(state.PriorSockets, ", ")))
	}
	if death, found, derr := tmux.ReadServerDeath(ctx.opts.ConfigDir); derr == nil && found {
		report.Info(sectionEnvironment, "tmux server last exit", death.Describe())
	}
}
