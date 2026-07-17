package session

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// Backend preconditions (#1933). docker, ssh, and hook each need things to be true
// before a session can run on them. Those requirements used to be stated only
// inside each runtime's Provision, so the only way to discover one was to create a
// session and read the failure.
//
// This file states them from the repo config and the daemon's own environment,
// with NO side effects and no provisioning, so a client can ask "could this repo
// use docker?" before offering the choice — and the runtimes call the same
// functions, so the message a user reads at choose time in the web is the message
// the CLI prints at create time.
//
// The rule these functions exist to enforce: a picker is a promise. Every backend
// offered as usable says "choose this and it will work". So availability is a
// CHECKED fact, never an assumed one — if a precondition cannot be verified, the
// answer is "unknown, and here is why", never a cheerful default. What can be
// checked here without side effects:
//
//	local  — nothing; it is always usable.
//	docker — docker.image set, the `docker` CLI on PATH, an `origin` to clone from.
//	ssh    — ssh.host set, an `origin` to clone from. (The transport is the Go ssh
//	         client, so there is no ssh CLI to look for; host reachability is a
//	         network fact this must not dial for.)
//	hook   — remote_hooks set AND valid AND its commands actually runnable.
//
// What is deliberately NOT checked, because it cannot be without doing the work:
// whether the image pulls, whether the ssh host is up and authorized, whether
// launch_cmd exits 0. Those still surface at create time. So "available" means
// "every precondition I can check without side effects passed" — a real promise,
// just not omniscience.

// lookPath resolves an executable the way exec.Command will when the runtime later
// runs it — same process, same PATH, so the answer here is the answer at create
// time. A package var so tests can drive the not-on-PATH branches hermetically
// (a test must not depend on whether the CI image ships a `docker` binary).
var lookPath = exec.LookPath

// SetLookPathForTest replaces the executable resolver with f and returns a restore
// function. Mirrors the SetRuntimeForTest / SetBackendFactoryForTest seam pattern.
func SetLookPathForTest(f func(string) (string, error)) func() {
	prev := lookPath
	lookPath = f
	return func() { lookPath = prev }
}

// BackendConfigError reports why kind's repo CONFIG is insufficient, or nil when
// the config satisfies it. This is the config-key half only — the runtimes call it
// as their first config-dependent check, and BackendUnusableReason calls it first
// too, so all three surfaces name the missing key in the same words.
//
// A nil cfg is treated as an empty config (every optional section absent), which is
// the correct reading for a repo with no in-repo config file. It is NOT the reading
// for a config that failed to LOAD — the caller must not conflate "no config" with
// "unreadable config"; see ListBackends, which reports the latter as unknown.
func BackendConfigError(kind BackendKind, cfg *config.ResolvedConfig) error {
	switch kind {
	case BackendDocker:
		if cfg == nil || cfg.Docker == nil || strings.TrimSpace(cfg.Docker.Image) == "" {
			return fmt.Errorf("backend=docker requires docker.image to be set in this repo's .agent-factory/config.json (the container image that carries git + tmux + the agent CLIs; the `af` binary is copied in automatically)")
		}
	case BackendSSH:
		if cfg == nil || cfg.SSH == nil || strings.TrimSpace(cfg.SSH.Host) == "" {
			return fmt.Errorf("backend=ssh requires ssh.host to be set in this repo's .agent-factory/config.json (the remote host the session's workspace + agent run on)")
		}
	case BackendHook:
		if cfg == nil || cfg.RemoteHooks == nil {
			return fmt.Errorf("backend=hook requires remote_hooks to be configured in this repo's .agent-factory/config.json (the launch/delete commands that provision the session on your own infrastructure)")
		}
	}
	// BackendLocal needs nothing from the repo config, and an unregistered kind is
	// not this function's error to raise — ParseBackendKind rejects those. A new
	// backend with no config requirement correctly reports no config error.
	return nil
}

// BackendUnusableReason reports why kind cannot be used for a session in the repo
// at repoRoot, or nil when every checkable precondition passes. cfg must be the
// repo's RESOLVED config (a caller that could not resolve it has an unknown
// answer, not a nil-cfg one).
//
// This is the choose-time question a picker must ask before offering a backend.
// It checks config keys, then the environment facts the runtime will need — a
// backend that is configured but whose command is missing is exactly the
// "offered, then fails later somewhere less obvious" trap this is here to close.
func BackendUnusableReason(kind BackendKind, cfg *config.ResolvedConfig, repoRoot string) error {
	if err := BackendConfigError(kind, cfg); err != nil {
		return err
	}

	switch kind {
	case BackendDocker:
		if _, err := lookPath("docker"); err != nil {
			return dockerCLIMissingError(err)
		}
		if originRemoteURL(repoRoot) == "" {
			return missingOriginError(BackendDocker, repoRoot)
		}
	case BackendSSH:
		if originRemoteURL(repoRoot) == "" {
			return missingOriginError(BackendSSH, repoRoot)
		}
	case BackendHook:
		// Validate() is what create runs (via loadRemoteHooksForPath): it catches an
		// empty launch_cmd/delete_cmd and a config still carrying the removed
		// pre-PR7 keys. Reusing it means "configured" can never again mean merely
		// "the section exists".
		if err := cfg.RemoteHooks.Validate(); err != nil {
			return fmt.Errorf("backend=hook: %w", err)
		}
		// The hook commands are exec'd directly as the program (not through a
		// shell), so resolving them is exactly what create will do. A hook whose
		// command is missing is the worst offender in this class: it is CONFIGURED,
		// so a config-only check calls it available, and the failure lands later,
		// mid-provision, as somebody else's error.
		for _, hook := range []struct{ key, cmd string }{
			{"launch_cmd", cfg.RemoteHooks.LaunchCmd},
			{"delete_cmd", cfg.RemoteHooks.DeleteCmd},
		} {
			if _, err := lookPath(hook.cmd); err != nil {
				return fmt.Errorf("backend=hook: remote_hooks.%s %q was not found on PATH (agent-factory runs it directly to provision the session); install it, or set an absolute path in this repo's %s", hook.key, hook.cmd, config.InRepoConfigFileName(repoRoot))
			}
		}
	}
	return nil
}

// dockerCLIMissingError is the one wording for "the docker CLI is absent", shared
// by the docker runtime (which hits it at create time) and BackendUnusableReason
// (which hits it at choose time).
func dockerCLIMissingError(err error) error {
	return fmt.Errorf("backend=docker: the `docker` CLI is not on PATH; install Docker or select a different backend: %w", err)
}

// missingOriginError is the one wording for "this repo has nothing to clone from",
// shared by the docker and ssh runtimes and by BackendUnusableReason. Both
// sandboxed runtimes clone the workspace from origin (epic decision 4: GitHub is
// the durable store), so neither can run in a repo that has no origin.
func missingOriginError(kind BackendKind, repoRoot string) error {
	return fmt.Errorf("backend=%s: repo %q has no `origin` remote to clone the workspace from; add one (GitHub is the durable workspace store) or push the repo first", kind, repoRoot)
}
