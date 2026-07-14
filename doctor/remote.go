package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// defaultRemoteConfig resolves the remote-hook backend for the repository
// containing the current working directory. It returns nil hooks — and no
// error — whenever remote checks should skip cleanly: cwd is not inside a git
// repo, or the repo configures no remote backend. This is the production
// resolver; tests inject their own via Options.remoteConfig to stay hermetic.
func defaultRemoteConfig() (*config.RemoteHooks, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	repo, err := config.RepoFromPath(cwd)
	if err != nil {
		// Not a git repo (or git unavailable): there is no repo whose
		// remote_hooks we could validate. Skip cleanly rather than fail.
		return nil, "", nil
	}
	cfg, err := config.ResolveConfig(repo.Root)
	if err != nil {
		return nil, repo.Root, err
	}
	// ResolveConfig has already rewritten relative hook paths to absolute
	// against repo.Root, so executability checks below can stat them directly.
	return cfg.RemoteHooks, repo.Root, nil
}

// checkRemoteSetup validates the remote-hook backend for the current repo:
// config completeness, hook-script presence/executability, and a bounded
// read-only connectivity probe via list_cmd. When no remote backend is
// configured (the common case for local-only users), it records a single
// informational "n/a" line and adds no findings — running `af doctor` outside
// a remote setup must produce zero new noise.
func checkRemoteSetup(ctx *scanContext, report *Report) {
	resolve := ctx.opts.remoteConfig
	if resolve == nil {
		resolve = defaultRemoteConfig
	}
	hooks, repoRoot, err := resolve()
	if err != nil {
		report.Warn(sectionRemote, "remote config", fmt.Sprintf("could not resolve remote-hook config for this repo: %v", err),
			"fix the repo .agent-factory config and rerun `af doctor`", false)
		return
	}
	if hooks == nil {
		report.Pass(sectionRemote, "remote hooks", "not configured for this repo")
		return
	}
	checkCoderStatus(hooks, report)

	configHint := "in [remote_hooks]"
	if repoRoot != "" {
		configHint = "in " + config.InRepoConfigPath(repoRoot) + " under [remote_hooks]"
	}

	// 1. Required-field validity: the same guard the backend enforces before
	// running any hook, surfaced here as a diagnosable finding instead of an
	// operation-time failure. Validate also rejects a config still carrying the
	// removed pre-PR7 keys (list_cmd/attach_cmd/terminal_cmd) with the migration
	// message, so `af doctor` names the exact stale key after an upgrade.
	if err := hooks.Validate(); err != nil {
		report.Warn(sectionRemote, "remote config", fmt.Sprintf("%v; set it %s", err, configHint),
			"edit the repo .agent-factory config and rerun `af doctor`", false)
	} else {
		report.Pass(sectionRemote, "remote config", "launch_cmd + delete_cmd configured")
	}

	// 2. Hook-script presence + executability, for every configured command
	// (#1592 Phase 4 PR7: the provision-and-expose contract is just launch_cmd +
	// delete_cmd). A path that exists but lacks the execute bit, or a bare name
	// missing from $PATH, is the most common remote-setup mistake — surface it
	// with the exact fix.
	type hook struct{ field, cmd string }
	for _, h := range []hook{
		{"launch_cmd", hooks.LaunchCmd},
		{"delete_cmd", hooks.DeleteCmd},
	} {
		if strings.TrimSpace(h.cmd) == "" {
			continue // required-field emptiness is handled by Validate above.
		}
		if detail := hookExecIssue(h.field, h.cmd, configHint); detail != "" {
			report.Warn(sectionRemote, "remote hook", detail,
				"fix the hook path or executable bit and rerun `af doctor`", false)
		}
	}

	// No connectivity round-trip probe: the provision-and-expose contract has no
	// read-only verb (launch_cmd provisions + starts an af agent-server,
	// delete_cmd tears it down — both mutate). The wire round-trip is exercised by
	// the daemon driving the exposed agent-server over http://, not by a doctor
	// probe. A dry-run of launch_cmd would provision real infrastructure, so
	// doctor deliberately does not run it.
}

func checkCoderStatus(hooks *config.RemoteHooks, report *Report) {
	mentionsCoder := remoteHooksMentionCoder(hooks)
	coderPath, lookErr := exec.LookPath("coder")
	if lookErr != nil {
		if mentionsCoder {
			report.Warn(sectionRemote, "coder", "coder CLI is not on PATH",
				"install coder or update the remote hook scripts", false)
		} else {
			report.Pass(sectionRemote, "coder", "not detected; hook commands do not directly reference coder")
		}
		return
	}
	if !mentionsCoder {
		report.Pass(sectionRemote, "coder", "available at "+coderPath)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, coderPath, "whoami")
	cmd.WaitDelay = 500 * time.Millisecond
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := "coder whoami failed"
		if ctx.Err() == context.DeadlineExceeded {
			detail = "coder whoami timed out"
		} else if line := firstLine(string(out)); line != "" {
			detail += ": " + line
		}
		report.Warn(sectionRemote, "coder", detail, "run `coder login`", false)
		return
	}
	who := firstLine(string(out))
	if who == "" {
		who = "authenticated"
	}
	report.Pass(sectionRemote, "coder", who)
}

func remoteHooksMentionCoder(hooks *config.RemoteHooks) bool {
	if hooks == nil {
		return false
	}
	for _, command := range []string{
		hooks.LaunchCmd,
		hooks.DeleteCmd,
	} {
		if strings.Contains(strings.ToLower(command), "coder") {
			return true
		}
	}
	return false
}

// hookExecIssue returns an actionable message when the hook command cannot be
// executed, or "" when it looks runnable. It mirrors exec.Command's own path
// semantics: a value with a path separator (or absolute) is a filesystem path
// that must exist and be executable; a bare name is resolved on $PATH. Values
// reaching here have already had relative paths rewritten to absolute by
// ResolveConfig, so a separator implies an absolute path.
func hookExecIssue(field, cmd, configHint string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.ContainsRune(cmd, filepath.Separator) {
		info, err := os.Stat(cmd)
		if err != nil {
			return fmt.Sprintf("remote_hooks.%s points at %s, which does not exist — "+
				"check the path %s", field, cmd, configHint)
		}
		if info.IsDir() {
			return fmt.Sprintf("remote_hooks.%s points at %s, which is a directory, not an "+
				"executable — check the path %s", field, cmd, configHint)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return fmt.Sprintf("remote_hooks.%s script %s is not executable — run: chmod +x %s",
				field, cmd, cmd)
		}
		return ""
	}
	// Bare name: resolved on $PATH exactly like exec.
	if _, err := exec.LookPath(cmd); err != nil {
		return fmt.Sprintf("remote_hooks.%s is %q, which was not found on $PATH — install it or "+
			"use a path to the script %s", field, cmd, configHint)
	}
	return ""
}

// firstLine returns the first non-empty line of s, for compact error context.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
