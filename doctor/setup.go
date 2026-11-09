package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func checkSetup(ctx *scanContext, report *Report) {
	checkAFHome(ctx, report)
	cfg := checkConfig(ctx, report)
	checkStateDirs(ctx, report)
	checkCodexAccounts(ctx.opts.ConfigDir, report)
	checkLogStorage(report)
	checkGit(report)
	checkTmux(report)
	if cfg != nil {
		checkAgentPrograms(cfg, report)
	}
	checkDaemonHealth(ctx, report, ctx.opts.daemonHealth(), cfg)
	checkRemoteSetup(ctx, report)
}

func checkAFHome(ctx *scanContext, report *Report) {
	if ctx.opts.ConfigDir == "" {
		report.addActionableFinding(Finding{
			Check:  "af-home",
			Detail: "agent-factory home could not be resolved",
		})
		return
	}
	if err := writableDir(ctx.opts.ConfigDir); err != nil {
		report.addActionableFinding(Finding{
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
			report.addActionableFinding(Finding{
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
		report.addActionableFinding(Finding{
			Check:  "log-storage",
			Detail: "could not resolve application log path — set AGENT_FACTORY_HOME to a writable directory and retry",
		})
		return
	}
	dir := filepath.Dir(path)
	if err := writableDir(dir); err != nil {
		report.addActionableFinding(Finding{
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
		report.addActionableFinding(Finding{
			Check:  "config",
			Detail: fmt.Sprintf("config is not valid — fix the reported file or delete it to regenerate defaults: %v", err),
		})
		return nil
	}
	path, err := config.GlobalConfigPath()
	if err != nil {
		report.addActionableFinding(Finding{
			Check:  "config",
			Detail: fmt.Sprintf("could not resolve config path: %v", err),
		})
		return cfg
	}
	if _, statErr := os.Stat(path); statErr != nil {
		report.addActionableFinding(Finding{
			Check:  "config",
			Detail: fmt.Sprintf("config loaded in memory but %s is not materialized on disk — run `%s` or fix AF home permissions: %v", path, shellsuggest.PositionalCommand("af", []string{"config", "set"}, "default_program", cfg.DefaultProgram), statErr),
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
		report.addActionableFinding(Finding{
			Check:  "git",
			Detail: "git is not installed or not on PATH — install git and retry",
		})
		return
	}
	report.Pass(sectionEnvironment, "git", fmt.Sprintf("available at %s", gitPath))

	cwd, err := os.Getwd()
	if err != nil {
		report.addActionableFinding(Finding{
			Check:  "git-repo",
			Detail: fmt.Sprintf("could not determine current directory: %v", err),
		})
		return
	}
	out, err := exec.Command(gitPath, "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		// Advice is worse than useless when the probe never answered: "run af
		// from a repo root" sends a user who is already at one somewhere else
		// (#3504). Classify through the same rule the resolver uses.
		if classified := config.ClassifyGitProbeError(context.Background(), err); config.RepoProbeUnanswered(classified) {
			report.addActionableFinding(Finding{
				Check:  "git-repo",
				Detail: fmt.Sprintf("%s — run `af doctor` again; if it persists, check that git runs here: %v", config.RepoProbeUnansweredClaim("the current directory", cwd), err),
				// The probe never answered, so this is NOT "not a repository": do
				// not send a user who may already be in a checkout to `git init`.
				Remediation: "rerun `af doctor --setup`; if it persists, check that git runs in this directory",
			})
			return
		}
		// An ANSWERED git failure is not necessarily a verdict about the path
		// (#3794's twin, which fixed the daemon's equivalent site without
		// updating the doctor). git ran and exited non-zero, but only the
		// specific "not a git repository (or any of the parent directories...)"
		// diagnostic — with no .git metadata present — says the path is outside
		// git. A safe.directory / dubious-ownership rejection, an unreadable
		// .git, an invalid .git file are all answered failures that say nothing
		// about whether this is a repository, so they must remain visible rather
		// than being mistaken for "outside git" (config/repo.go,
		// ErrNotGitRepository doc). Classify through the resolver's rule, the
		// same one the daemon uses, instead of the bare fall-through that
		// treated every answered failure as "not a repository".
		_, repoErr := config.RepoFromPath(cwd)
		if repoErr == nil {
			// The toplevel probe refused but the resolver still accepted
			// the path: a bare repository has no work tree, so
			// --show-toplevel exits 128 while the resolver's direct-bare
			// fallback recovers the bare root. Treat it as the valid
			// repository the resolver says it is so the identity check
			// runs instead of being silently skipped (#6954 review).
			report.Pass(sectionConfig, "git repo", cwd)
			checkGitIdentity(gitPath, cwd, report)
			return
		}
		if errors.Is(repoErr, config.ErrNotGitRepository) {
			report.addActionableFinding(Finding{
				Check:       "git-repo",
				Detail:      fmt.Sprintf("%s is not inside a git repository — run `af` from a repo root or pass --repo where supported", cwd),
				Remediation: "cd into a git repository (or run `git init` in your project), then rerun `af doctor --setup`",
			})
			return
		}
		// Answered, but the answer says nothing about whether this is a
		// repository: keep the failure visible without "git init" advice, which
		// is misleading for a checkout that already exists.
		// Format the captured stderr, not just "exit status 128", so the user
		// can see git's own diagnostic (dubious ownership, unreadable .git,
		// ...) and pick the matching remedy (#6954 review).
		gitErrDetail := fmt.Sprintf("%v", err)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				gitErrDetail = stderr
			}
		}
		report.addActionableFinding(Finding{
			Check:       "git-repo",
			Detail:      fmt.Sprintf("git could not resolve a repository root at %s — git's error: %s", cwd, gitErrDetail),
			Remediation: "address the git error above (e.g. `git config --global --add safe.directory <path>` for an ownership refusal), then rerun `af doctor --setup`",
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
		report.addActionableFinding(Finding{
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
		report.addActionableFinding(Finding{Check: "tmux", Detail: err.Error()})
		return
	}
	report.Pass(sectionEnvironment, "tmux", version)
}

func checkAgentPrograms(cfg *config.Config, report *Report) {
	// Resolve the effective config the way session launch does: when the doctor
	// is inside a git repo, merge the in-repo layer over the global config so
	// the setup profile verifies the first session's actual prerequisites, not
	// the global layer alone. Outside a repo the global config is the only
	// layer sessions could launch under, so it is kept unchanged.
	repo, repoErr := currentRepoContext()
	workspace := ""
	if repoErr == nil {
		workspace = repo.Root
	}
	// Classify the repo probe the way checkGit does (#3794's twin): only
	// ErrNotGitRepository is a verdict that the path is outside git, so only
	// it entitles the agent checks to fall back to the global config layer.
	// An answered git refusal (dubious ownership, an invalid .git) or an
	// unanswered probe says nothing about whether this is a repository, so
	// the in-repo layer and its backend could not be inspected — the global
	// layer's programs must not be silently validated, because the repo's
	// sessions will not launch them (config/repo.go, ErrNotGitRepository doc).
	//
	// In the --setup flow, checkGit (above) has already classified this same
	// probe and recorded the matching git/git-repo finding. Adding a second
	// agent-program finding here would double-count one underlying git
	// failure in UnresolvedCount and the summary, so reuse that finding:
	// when checkGit already reported the probe, skip the program checks
	// without a new finding. When checkAgentPrograms runs in isolation (no
	// prior git/git-repo finding), emit the agent-program finding so the
	// failure is never silent (#6995 review).
	if repoErr != nil && !errors.Is(repoErr, config.ErrNotGitRepository) {
		if report.hasActionableFindingByCheck("git", "git-repo") {
			return
		}
		cwd := "<unknown>"
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
		detail, remediation := repoNotInspectableDetail(cwd, repoErr, "af doctor --setup")
		report.addActionableFinding(Finding{
			Check:       "agent-program",
			Detail:      detail,
			Remediation: remediation,
		})
		return
	}
	effective, backend, resolved, resolveErr := resolveEffectiveConfig(cfg, repo)
	if resolveErr != nil {
		// A repo whose in-repo config exists but cannot be resolved refuses
		// session creation with this same error (#5801). Report it as an
		// actionable finding instead of checking a command the session would
		// never launch. The resolver error names the offending layer, so the
		// remediation stays layer-neutral rather than unconditionally pointing
		// at the checked-in in-repo file.
		report.addActionableFinding(Finding{
			Check: "agent-program",
			Detail: fmt.Sprintf("could not resolve config for the repo at %s — fix the config layer named in this error and rerun `af doctor --setup`: %v",
				repo.Root, resolveErr),
		})
		return
	}
	if effective == nil || effective.DefaultProgram == "" {
		report.addActionableFinding(Finding{
			Check:  "agent-program",
			Detail: fmt.Sprintf("default_program is empty — set it to one of %s in ~/.agent-factory/config.toml", tmux.SupportedProgramsString()),
		})
		return
	}
	if !isLocalBackend(backend) {
		// Validate the selector and the backend's preconditions the way session
		// creation does. An unknown backend value (e.g. `backend = "dokcer"`)
		// is a misconfiguration af refuses at create time, not a remote backend
		// to skip host checks for; and a supported but unusable one (docker
		// without docker.image or a docker CLI, ssh without ssh.host or an
		// `origin` remote, hook without runnable remote_hooks) is rejected by
		// BackendUnusableReason — the same side-effect-free check a default
		// create fails. Either way report an actionable finding instead of a
		// silent skip that exits cleanly for a repo whose every default create
		// would fail (#6792 item 4).
		kind, kerr := session.ParseBackendKind(backend)
		if kerr != nil {
			report.addActionableFinding(Finding{
				Check: "agent-program",
				Detail: fmt.Sprintf("unknown backend %q in the in-repo config: %v — set `backend` to one of %s in the in-repo config (.agent-factory/config.toml) and rerun `af doctor --setup`",
					backend, kerr, config.SupportedBackendsString()),
			})
			return
		}
		if resolved != nil {
			if reason := session.BackendUnusableReason(kind, resolved, repo.Root); reason != nil {
				report.addActionableFinding(Finding{
					Check: "agent-program",
					Detail: fmt.Sprintf("backend %q is not usable for sessions in this repo: %v — address the problem named above and rerun `af doctor --setup`",
						backend, reason),
				})
				return
			}
		}
		// docker/ssh/hook/sandbox run the agent off the daemon host, so the
		// host's agent binaries are not prerequisites for them — mirroring
		// daemon.createNeedsDaemonHostPrereqs. Report a non-actionable row so
		// the setup profile records why no binary check ran: an informational
		// row, not a PASS for a check that never ran.
		report.Info(sectionEnvironment, "agent-program",
			fmt.Sprintf("host agent binaries not checked — this repo's sessions run on the %q backend", backend))
		return
	}
	// Anchor relative program_overrides to the session's launch directory (the
	// repo the doctor is inside), never the doctor's own cwd. The daemon's
	// create-validation path does the same with repo.WorkspacePath() (#5779);
	// the doctor was left behind, so the same relative override flipped
	// between PASS and FAIL purely on where `af doctor` was typed. Outside any
	// repo the launch directory cannot be determined, so today's CheckCommand
	// is kept and any failure is reported with an honest message instead of
	// install/reinstall guidance (#6487).
	checkOneProgram(workspace, repoErr, "default_program", effective.DefaultProgram, config.ResolveProgram(effective, effective.DefaultProgram), report)
	for _, agent := range sortedKeys(effective.ProgramOverrides) {
		command := strings.TrimSpace(effective.ProgramOverrides[agent])
		if command == "" {
			continue
		}
		checkOneProgram(workspace, repoErr, "program_overrides."+agent, agent, command, report)
	}
}

func checkOneProgram(workspace string, repoErr error, field, agent, command string, report *Report) {
	check, err, outsideRepo := checkProgramCommand(workspace, repoErr, command)
	if err != nil {
		detail := fmt.Sprintf("%s resolves to %q but is not runnable — %v", field, command, preflight.ProgramError(agent, command, err))
		if outsideRepo {
			detail = noAnchorMessage(command)
		}
		report.addActionableFinding(Finding{
			Check:  "agent-program",
			Detail: detail,
		})
		return
	}
	// A resolution that depended on the launch directory and landed inside the
	// repo is honest only when the tree a session branches from — origin's
	// default branch, else HEAD — holds a runnable file at the same
	// repo-relative path: the worktree is a clean checkout of that tree, not
	// of this checkout's index or working tree (#6499, #6792). Downgrade the
	// PASS to an advisory WARN so the doctor does not report a
	// guaranteed-false PASS for a file the session's worktree will not
	// contain.
	if detail, remediation := overrideAbsentFromSessionBase(workspace, check); detail != "" {
		report.Warn(sectionEnvironment, field, detail, remediation, false)
		return
	}
	report.Pass(sectionEnvironment, field, fmt.Sprintf("%q uses %s", command, check.Path))
}

func writableDir(dir string) error {
	// Through the home guard: every caller passes the AF home or a directory
	// inside it (#3850). `af doctor` is a CLI process and never arms the latch,
	// so this is an exact os.MkdirAll — but see the note in app/detach_trace.go
	// on why the routing is unconditional.
	if err := config.MkdirAllUnderAFHome(dir, 0o755); err != nil {
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
