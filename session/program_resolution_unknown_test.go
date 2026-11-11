package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7077: #6748 made the config layer say "slow is not absent" — a checkout
// probe that outlives its budget fails the resolve instead of fabricating an
// empty personal layer — and the session layer's first caller undid it by
// treating every resolve error as "use the global config". These pin the
// property per caller class: a resolve whose answer launches or configures an
// agent never acts on the global config because the project's could not be
// read in time; "absent" (not a repository, or a config the repo answered as
// invalid) still resolves exactly as before.

const (
	globalAiderCommand  = "/opt/global/aider"
	projectAiderCommand = "/opt/project/aider"
)

// unansweredProbe is the shape #6748 propagates and the #7077 log line shows:
// the checkout-marker read was still unanswered when the scan budget ended.
func unansweredProbe(path string) error {
	return fmt.Errorf("read checkout marker for %s: %w", path, context.DeadlineExceeded)
}

// projectAiderConfig is the project's resolved answer: its own aider override,
// distinct from the global one, so a test can tell which config a launch used.
func projectAiderConfig() *config.ResolvedConfig {
	resolved := &config.ResolvedConfig{}
	resolved.Config.ProgramOverrides = map[string]string{tmux.ProgramAider: projectAiderCommand}
	return resolved
}

// globalAiderHome gives the test a fresh af home whose GLOBAL config routes
// aider to globalAiderCommand — the answer a launch must not fall back to.
func globalAiderHome(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	if cfg.ProgramOverrides == nil {
		cfg.ProgramOverrides = map[string]string{}
	}
	cfg.ProgramOverrides[tmux.ProgramAider] = globalAiderCommand
	require.NoError(t, config.SaveConfig(cfg))
}

// launchInstanceResolving returns an aider instance with a worktree whose
// project-config resolution is answered by resolve (called with the 1-based
// attempt number). The batch restore context is the existing injection seam
// every instance-scoped resolver already reads.
func launchInstanceResolving(t *testing.T, resolve func(attempt int32) (*config.ResolvedConfig, error)) (*Instance, *atomic.Int32) {
	t.Helper()
	globalAiderHome(t)
	work := t.TempDir()
	gw, err := git.NewGitWorktreeFromStorage(work, work, "unknown-config", "main", "", true, false)
	require.NoError(t, err)
	inst := &Instance{Title: "unknown-config", Path: work, Program: tmux.ProgramAider, gitWorktree: gw}

	var attempts atomic.Int32
	rc := NewRestoreContext()
	rc.repoFromPath = func(path string) (*config.RepoContext, error) {
		return &config.RepoContext{Root: path, IdentityRoot: path, ID: "id-unknown-config"}, nil
	}
	rc.resolveConfig = func(repo *config.RepoContext) (*config.ResolvedConfig, error) {
		n := attempts.Add(1)
		return resolve(n)
	}
	inst.restoreCtx.Store(rc)
	return inst, &attempts
}

// requireActionableUnknownConfigError pins property 5: the refusal says what
// happened and what to do, names the path, and keeps the probe's cause in the
// chain — never a bare `context deadline exceeded`.
func requireActionableUnknownConfigError(t *testing.T, err error, path string) {
	t.Helper()
	require.Error(t, err, "an unknown project config must refuse the launch, not resolve the global config")
	require.ErrorIs(t, err, context.DeadlineExceeded, "the probe's cause stays inspectable")
	msg := err.Error()
	require.Contains(t, msg, path, "the error names the project it could not read")
	require.Contains(t, msg, "in time", "the error says the read ran out of time, not that the config is wrong")
	require.Contains(t, msg, "retry", "the error says what to do")
	require.Contains(t, msg, "global config", "the error says af deliberately did not substitute the global config")
	require.NotEqual(t, context.DeadlineExceeded.Error(), msg)
}

func TestCreateLaunchRefusesTheGlobalProgramWhenTheProjectConfigReadTimesOut(t *testing.T) {
	inst, attempts := launchInstanceResolving(t, func(int32) (*config.ResolvedConfig, error) {
		return nil, unansweredProbe("/repo")
	})

	plan, err := (&LocalBackend{}).prepareCreateLaunch(inst)

	require.NotEqual(t, globalAiderCommand, plan.baseProgram,
		"the launch must never freeze the global override when the project's could not be read")
	requireActionableUnknownConfigError(t, err, inst.Path)
	require.Greater(t, attempts.Load(), int32(1), "a launch spends more than one probe budget before refusing")
}

func TestCreateLaunchRetriesATransientProjectConfigTimeout(t *testing.T) {
	inst, attempts := launchInstanceResolving(t, func(attempt int32) (*config.ResolvedConfig, error) {
		if attempt == 1 {
			return nil, unansweredProbe("/repo")
		}
		return projectAiderConfig(), nil
	})

	plan, err := (&LocalBackend{}).prepareCreateLaunch(inst)

	require.NoError(t, err, "a probe that answers on a later attempt is the project's answer")
	require.Equal(t, projectAiderCommand, plan.baseProgram, "the launch uses the project's override, not the global one")
	require.EqualValues(t, 2, attempts.Load())
}

// Property 2: "absent" is still absent. A config the repository answered as
// invalid keeps today's warn-and-use-global degrade — it was an answer, not a
// timeout. (That it is not retried is pinned on the resolver itself, below.)
func TestCreateLaunchKeepsTheGlobalFallbackForAnAnsweredResolveError(t *testing.T) {
	inst, _ := launchInstanceResolving(t, func(int32) (*config.ResolvedConfig, error) {
		return nil, errors.New("parse .agent-factory/config.toml: toml: expected '=' at line 3")
	})

	plan, err := (&LocalBackend{}).prepareCreateLaunch(inst)

	require.NoError(t, err)
	require.Equal(t, globalAiderCommand, plan.baseProgram)
}

func TestCreateLaunchOutsideARepositoryStillUsesTheGlobalConfig(t *testing.T) {
	inst, attempts := launchInstanceResolving(t, func(int32) (*config.ResolvedConfig, error) {
		t.Fatal("no repository means no project config to resolve")
		return nil, nil
	})
	inst.restoreContext().repoFromPath = func(path string) (*config.RepoContext, error) {
		return nil, fmt.Errorf("%w: fatal: not a git repository (or any of the parent directories): .git", config.ErrNotGitRepository)
	}

	plan, err := (&LocalBackend{}).prepareCreateLaunch(inst)

	require.NoError(t, err)
	require.Equal(t, globalAiderCommand, plan.baseProgram)
	require.Zero(t, attempts.Load())
}

// The local backend's environment is the second resolve on every start and
// restore (#7077's backend_local_environment.go:29): the pane's pass-through
// allowlist must come from the project's config or not at all.
func TestSessionEnvironmentRefreshRefusesWhenTheProjectConfigReadTimesOut(t *testing.T) {
	inst, _ := launchInstanceResolving(t, func(int32) (*config.ResolvedConfig, error) {
		return nil, unansweredProbe("/repo")
	})
	ts := tmux.NewTmuxSessionForRepo(inst.Title, inst.Path, inst.Program)

	err := refreshSessionEnvironment(inst, ts)

	requireActionableUnknownConfigError(t, err, inst.Path)
}

// slowCheckoutProbeRepo is a registered project whose checkout-identity probe —
// `git -C <root> rev-parse --git-common-dir`, the call #6748 bounds with its 1s
// scan — sleeps past that budget, while every other git call answers normally.
// It is the real #7077 shape end to end: the config layer returns "read
// checkout marker for <repo>: context deadline exceeded", and the resolvers
// below are reached through config.ResolveConfigForRepo with no seam at all.
func slowCheckoutProbeRepo(t *testing.T) string {
	t.Helper()
	globalAiderHome(t)
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initCmd := exec.Command("git", "init", "--quiet")
	initCmd.Dir = repo
	initCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	repo, err = filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	_, err = config.RegisterProject(repo)
	require.NoError(t, err)

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$#" -eq 4 ] && [ "$3" = rev-parse ] && [ "$4" = --git-common-dir ]; then
	sleep 3
fi
exec %q "$@"
`, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return repo
}

// Handoff admission resolves the target's command through the path-scoped
// chain (#7077's handoff.go:360) — the same chain the daemon's agent-server
// create uses through ResolveProgramForPath.
func TestHandoffTargetValidationRefusesWhenTheProjectConfigReadTimesOut(t *testing.T) {
	repo := slowCheckoutProbeRepo(t)
	inst := &Instance{Title: "slow-probe", Path: repo, Program: tmux.ProgramClaude}

	err := inst.ValidateHandoffTarget(tmux.ProgramCodex)

	requireActionableUnknownConfigError(t, err, repo)
}

// The account gate reads the same overrides the launch will, so it must refuse
// with the launch rather than admit an account against the global overrides.
func TestAccountAgentGateRefusesWhenTheProjectConfigReadTimesOut(t *testing.T) {
	repo := slowCheckoutProbeRepo(t)

	err := refuseUnsupportedAccountAgent(InstanceOptions{
		Title: "slow-probe", Path: repo, Program: tmux.ProgramClaude, Account: "work",
	}, repo)

	requireActionableUnknownConfigError(t, err, repo)
}

// The create path's first resolve picks the backend. It already refused an
// unresolvable config (#5801), but told the user to "fix the config" — wrong
// advice for a read that only ran out of time.
func TestBackendSelectionNamesATimedOutProjectConfigReadAsRetryable(t *testing.T) {
	repo := slowCheckoutProbeRepo(t)

	_, err := resolveBackendKind(InstanceOptions{}, repo)

	requireActionableUnknownConfigError(t, err, repo)
	require.NotContains(t, err.Error(), "fix the config", "nothing is wrong with the config; it was not read in time")
}

// Property 3: a read-only picker may keep its fallback, but the substitution
// must be named where an operator can see it, not reported as a bare resolve
// failure.
func TestInspectionFallbackNamesTheGlobalSubstitution(t *testing.T) {
	repo := slowCheckoutProbeRepo(t)
	var buf bytes.Buffer
	prev := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&buf)
	t.Cleanup(func() { log.WarningLog.SetOutput(prev) })

	resolved := HandoffEffectiveAgentsForPathInspection(repo, []string{tmux.ProgramClaude})

	require.Contains(t, resolved, tmux.ProgramClaude, "a picker still gets an answer")
	require.Contains(t, buf.String(), repo)
	require.Contains(t, buf.String(), "global config", "the warning names what the answer was substituted from")
}

// The helpers below adapt pre-#7077 tests to the resolvers' error returns: in
// each of those tests the project config is readable, so an error is a failure.

func effectiveAgentForTest(t *testing.T, path, target string) string {
	t.Helper()
	agent, err := HandoffEffectiveAgentForPath(path, target)
	require.NoError(t, err)
	return agent
}

func instanceEffectiveAgentForTest(t *testing.T, i *Instance, target string) string {
	t.Helper()
	agent, err := handoffEffectiveAgent(i, target)
	require.NoError(t, err)
	return agent
}

func launchResolutionForTest(t *testing.T, i *Instance) launchProgramResolution {
	t.Helper()
	resolution, err := resolveLaunchProgramForInstance(i)
	require.NoError(t, err)
	return resolution
}

func manualSwapProgramForTest(t *testing.T, i *Instance, agent string, accountOnly bool) (string, bool) {
	t.Helper()
	program, cross, err := i.ManualAccountSwapProgram(agent, accountOnly)
	require.NoError(t, err)
	return program, cross
}

// The resolver's own contract, which the end-to-end tests above rely on:
// exactly launchConfigResolveAttempts unanswered attempts, an answer is final
// on the first try, and an unanswered repository probe is unknown too.

func noRetryPause(t *testing.T) {
	t.Helper()
	prev := launchConfigRetryPause
	launchConfigRetryPause = 0
	t.Cleanup(func() { launchConfigRetryPause = prev })
}

func TestResolveProjectConfigSpendsExactlyItsAttemptsOnAnUnknown(t *testing.T) {
	noRetryPause(t)
	var calls int
	src := projectConfigSource{
		repoFromPath: func(path string) (*config.RepoContext, error) { return &config.RepoContext{Root: path}, nil },
		resolveRepo: func(*config.RepoContext) (*config.ResolvedConfig, error) {
			calls++
			return nil, unansweredProbe("/repo")
		},
	}

	resolved, err := resolveProjectConfig("/repo", src)

	require.Nil(t, resolved)
	var unknown *ProjectConfigUnknownError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, launchConfigResolveAttempts, calls)
	require.Equal(t, launchConfigResolveAttempts, unknown.Attempts)
	require.Contains(t, err.Error(), fmt.Sprintf("%d attempts", launchConfigResolveAttempts))
}

func TestResolveProjectConfigDoesNotRetryAnAnswer(t *testing.T) {
	noRetryPause(t)
	var calls int
	invalid := errors.New("parse config.toml: toml: expected '='")
	src := projectConfigSource{
		repoFromPath: func(path string) (*config.RepoContext, error) { return &config.RepoContext{Root: path}, nil },
		resolveRepo: func(*config.RepoContext) (*config.ResolvedConfig, error) {
			calls++
			return nil, invalid
		},
	}

	resolved, err := resolveProjectConfig("/repo", src)

	require.Nil(t, resolved)
	require.ErrorIs(t, err, invalid, "an answered failure is returned as itself")
	require.False(t, isProjectConfigUnknown(err))
	require.Equal(t, 1, calls)
}

// A repository probe that could not run (fork failure, a signal) has not said
// "not a repository": the path may be a project with its own config, so it is
// retried and then unknown — never the global config's answer.
func TestResolveProjectConfigTreatsAnUnansweredRepoProbeAsUnknown(t *testing.T) {
	noRetryPause(t)
	var calls int
	src := projectConfigSource{
		repoFromPath: func(string) (*config.RepoContext, error) {
			calls++
			return nil, fmt.Errorf("failed to get git repo root: %w", errors.Join(config.ErrRepoProbeUnanswered, errors.New("fork/exec git: resource temporarily unavailable")))
		},
		resolveRepo: func(*config.RepoContext) (*config.ResolvedConfig, error) {
			t.Fatal("no repository was resolved")
			return nil, nil
		},
	}

	resolved, err := resolveProjectConfig("/repo", src)

	require.Nil(t, resolved)
	require.True(t, isProjectConfigUnknown(err))
	require.ErrorIs(t, err, config.ErrRepoProbeUnanswered)
	require.Equal(t, launchConfigResolveAttempts, calls)
	require.Contains(t, err.Error(), "git did not answer", "a probe that never ran is not reported as a timeout")
}

func TestResolveProjectConfigOutsideARepositoryIsAbsent(t *testing.T) {
	noRetryPause(t)
	var calls int
	src := projectConfigSource{
		repoFromPath: func(string) (*config.RepoContext, error) {
			calls++
			return nil, fmt.Errorf("%w: fatal: not a git repository", config.ErrNotGitRepository)
		},
		resolveRepo: func(*config.RepoContext) (*config.ResolvedConfig, error) {
			t.Fatal("no repository was resolved")
			return nil, nil
		},
	}

	resolved, err := resolveProjectConfig("/tmp/plain", src)

	require.NoError(t, err, "outside a repository there is no project config to be unknown")
	require.Nil(t, resolved)
	require.Equal(t, 1, calls)
}
