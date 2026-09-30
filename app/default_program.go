package app

import (
	"context"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// programChoice is the program new sessions and tasks default to, and where
// that value comes from. It is embedded in home, so m.program reads as before.
type programChoice struct {
	// program is the last-known default. When programFollowsConfig is set it is
	// only a cache of the configured default_program, refreshed synchronously by
	// refreshDefaultProgram; otherwise it is an explicit choice (the launch
	// --program flag) that config changes must not override.
	program string
	// programFollowsConfig reports that program was derived from the
	// default_program config key rather than chosen explicitly. `af config set
	// default_program` applies live (#2480), so a value derived from config is
	// re-read whenever it is about to be used instead of trusting the boot-time
	// snapshot (#4889).
	programFollowsConfig bool
}

// newProgramChoice seeds the launch choice. An empty program means no --program
// flag was given, so the default follows config. configuredDefault is the
// launch-time RESOLVED default_program — the active project's own value when
// launched inside a repo — so a failed re-read falls back to the value launch
// actually resolved rather than the bare global default; appConfig's global
// value is only the last resort for a caller that never ran launch's
// resolution.
func newProgramChoice(program, configuredDefault string, appConfig *config.Config) programChoice {
	if program != "" {
		return programChoice{program: program}
	}
	seed := configuredDefault
	if seed == "" {
		seed = appConfig.DefaultProgram
	}
	return programChoice{program: seed, programFollowsConfig: true}
}

// defaultProgram returns the program a new session or task defaults to: the
// last-known default_program for the active project. It is a CACHE READ only —
// callers that need the live value run refreshDefaultProgram first.
func (m *home) defaultProgram() string {
	return m.program
}

// localDefaultProgramTimeout bounds the local default_program re-read. The git
// identity probe inside RepoFromPath is the part that can wedge on a stale
// mount or a broken checkout; bound it so a stuck probe fails the lookup —
// keeping the last-known cache — instead of pinning the event loop forever.
// A var so tests can shorten the bound — the timeout path it arms is the
// regression under test, and waiting ten seconds for it would out-slow the
// failure it exists to catch.
var localDefaultProgramTimeout = 10 * time.Second

// resolveConfiguredProgramContext reads default_program for the given project
// root, or from the global config alone when none is active (registry mode).
// The context bounds the git probe; the TOML reads after it run under the same
// context inside ResolveConfigForRepoContext.
//
// A package var so tests can stub the filesystem read.
var resolveConfiguredProgramContext = func(ctx context.Context, repoRoot string) (string, error) {
	if repoRoot == "" {
		cfg, err := config.LoadConfig()
		if err != nil {
			return "", err
		}
		return cfg.DefaultProgram, nil
	}
	repo, err := config.RepoFromPathContext(ctx, repoRoot)
	if err != nil {
		return "", err
	}
	resolved, err := config.ResolveConfigForRepoContext(ctx, repo)
	if err != nil {
		return "", err
	}
	return resolved.DefaultProgram, nil
}

// refreshDefaultProgram re-reads the live default_program for a LOCAL target
// and refills the cache when it succeeds (#4889). It runs synchronously on the
// event loop, matching the TUI's other synchronous config reads (the project
// switch's ResolveConfigForRepo, the preflight's resolveBackendKind, the global
// LoadConfig in newHome): the read is a bounded local TOML probe whose common
// case is microseconds, and whose worst case is capped by
// localDefaultProgramTimeout — strictly cheaper than the unbounded precedent.
//
// A read error keeps the last-known value rather than blanking the field.
//
// A REMOTE daemon keeps master's behaviour: the field shows the launch-time
// default, and no read happens at all — the remote daemon's default_program is
// remote state the client's local config files cannot see, and there is no
// synchronous remote read to make.
func (m *home) refreshDefaultProgram() {
	if !m.programFollowsConfig || isRemoteTarget() {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, localDefaultProgramTimeout)
	defer cancel()
	program, err := resolveConfiguredProgramContext(ctx, m.repoRoot)
	if err != nil {
		log.WarningLog.Printf("default program: live default unreadable, keeping %q: %v", m.program, err)
		return
	}
	if program != "" {
		m.program = program
	}
}
