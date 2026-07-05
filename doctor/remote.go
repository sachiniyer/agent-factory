package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// remoteProbeTimeoutDefault bounds the connectivity probe. The probe runs the
// user's list_cmd (which may SSH to a remote host), so it is generous relative
// to the runtime IsAlive timeouts (2s/5s): a user running `af doctor` is
// actively diagnosing and can tolerate a slow round-trip, and the goal is an
// actionable "did not respond" rather than a fast failure on a slow link.
const remoteProbeTimeoutDefault = 10 * time.Second

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
		report.Findings = append(report.Findings, Finding{
			Check:  "remote-config",
			Detail: fmt.Sprintf("could not resolve remote-hook config for this repo: %v", err),
		})
		return
	}
	if hooks == nil {
		report.OK = append(report.OK,
			"remote hooks: n/a — no remote backend configured for this repo")
		return
	}

	configHint := "in [remote_hooks]"
	if repoRoot != "" {
		configHint = "in " + config.InRepoConfigPath(repoRoot) + " under [remote_hooks]"
	}

	// 1. Required-field validity: the same guard the backend enforces before
	// running any hook, surfaced here as a diagnosable finding instead of an
	// operation-time failure.
	if err := hooks.Validate(); err != nil {
		report.Findings = append(report.Findings, Finding{
			Check:  "remote-config",
			Detail: fmt.Sprintf("%v — set it %s", err, configHint),
		})
	} else {
		report.OK = append(report.OK,
			"remote hooks: configured with the required launch/attach/delete commands")
	}

	// 2. Hook-script presence + executability, for every configured command.
	// A path that exists but lacks the execute bit, or a bare name missing
	// from $PATH, is the most common remote-setup mistake — surface it with
	// the exact fix.
	type hook struct{ field, cmd string }
	for _, h := range []hook{
		{"launch_cmd", hooks.LaunchCmd},
		{"list_cmd", hooks.ListCmd},
		{"attach_cmd", hooks.AttachCmd},
		{"delete_cmd", hooks.DeleteCmd},
		{"terminal_cmd", hooks.TerminalCmd},
	} {
		if strings.TrimSpace(h.cmd) == "" {
			continue // required-field emptiness is handled by Validate above.
		}
		if detail := hookExecIssue(h.field, h.cmd, configHint); detail != "" {
			report.Findings = append(report.Findings, Finding{
				Check:  "remote-hook-script",
				Detail: detail,
			})
		}
	}

	// 3. Connectivity / round-trip probe. list_cmd is the only read-only verb
	// (launch/attach/delete all mutate remote state), so it is the safe probe.
	// When list_cmd is absent, restore and this probe are both unavailable —
	// noted as informational, not a failure, since list_cmd is optional.
	if strings.TrimSpace(hooks.ListCmd) == "" {
		report.OK = append(report.OK,
			"remote hooks: connectivity probe skipped — no list_cmd configured "+
				"(set list_cmd to enable restore across restarts and the round-trip probe)")
		return
	}
	// Skip the probe if list_cmd itself is not runnable — the executability
	// check above already reported the actionable fix; running it would only
	// add a redundant exec error.
	if hookExecIssue("list_cmd", hooks.ListCmd, configHint) != "" {
		return
	}
	timeout := ctx.opts.remoteProbeTimeout
	if timeout == 0 {
		timeout = remoteProbeTimeoutDefault
	}
	if detail := probeListCmd(hooks.ListCmd, timeout); detail != "" {
		report.Findings = append(report.Findings, Finding{
			Check:  "remote-connectivity",
			Detail: detail,
		})
	} else {
		report.OK = append(report.OK,
			"remote hooks: connectivity probe succeeded (list_cmd responded with valid JSON)")
	}
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

// probeListCmd runs `list_cmd --json` under a bounded timeout and returns an
// actionable message on failure, or "" when the round-trip succeeded (exit 0
// and a parseable JSON array). stdout and stderr are captured separately so
// the JSON parse sees only the documented stdout payload while error messages
// can quote the script's stderr diagnostics.
func probeListCmd(listCmd string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, listCmd, "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Bound how long the reader waits after a timeout kill, mirroring the
	// runtime list_cmd path (#645): a script that spawned a long-lived child
	// must not hold the probe open past the deadline.
	cmd.WaitDelay = 500 * time.Millisecond

	err := cmd.Run()

	// Judge the outcome by the process's ACTUAL exit status, not merely by
	// whether cmd.Run returned an error. Run can return a benign non-nil error
	// even when the command SUCCEEDED — notably exec.ErrWaitDelay, which fires
	// when a spawned grandchild keeps a stdout/stderr pipe open past WaitDelay
	// although the list script itself already exited 0. Treating any err != nil
	// as a connectivity failure would make doctor cry wolf about a working
	// remote — the worst outcome for a diagnostic (#1234 review). ExitCode is
	// -1 when the process was signalled (e.g. killed on our timeout) or never
	// started.
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	switch {
	case ctx.Err() == context.DeadlineExceeded && exitCode != 0:
		// Deadline fired and the command did not already finish cleanly.
		return fmt.Sprintf("remote_hooks.list_cmd did not respond within %s — the remote may be "+
			"unreachable; check connectivity (e.g. the SSH host in %s)", timeout, listCmd)
	case exitCode > 0:
		// Genuine command failure: a non-zero exit.
		msg := fmt.Sprintf("remote_hooks.list_cmd exited %d", exitCode)
		if s := strings.TrimSpace(stderr.String()); s != "" {
			msg += ": " + firstLine(s)
		}
		return msg + " — the remote round-trip is not working; run `" + listCmd +
			" --json` by hand to see the full output"
	case exitCode < 0:
		// The process never produced an exit status (failed to start, or was
		// signalled for a reason other than our deadline) — a real failure.
		msg := fmt.Sprintf("remote_hooks.list_cmd failed to run (%v)", err)
		if s := strings.TrimSpace(stderr.String()); s != "" {
			msg += ": " + firstLine(s)
		}
		return msg
	}

	// exitCode == 0: the command succeeded. Any err here (e.g. ErrWaitDelay) is
	// benign and must NOT be reported as a failure. Validate the stdout payload
	// against the documented contract: a JSON array of session objects.
	var sessions []map[string]interface{}
	if jsonErr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &sessions); jsonErr != nil {
		return fmt.Sprintf("remote_hooks.list_cmd exited 0 but did not return a JSON array on stdout "+
			"(got %q) — see docs/remote-hooks.md for the list_cmd protocol", snippet(stdout.String()))
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

// snippet trims and truncates s for inclusion in a finding message.
func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}
