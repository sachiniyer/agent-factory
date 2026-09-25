package app

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// programChoice is the program new sessions and tasks default to, and where
// that value comes from. It is embedded in home, so m.program reads as before.
type programChoice struct {
	// program is the last-known default. When programFollowsConfig is set it is
	// only a cache of the configured default_program, refreshed by
	// defaultProgram; otherwise it is an explicit choice (the launch --program
	// flag) that config changes must not override.
	program string
	// programFollowsConfig reports that program was derived from the
	// default_program config key rather than chosen explicitly. `af config set
	// default_program` applies live (#2480), so a value derived from config is
	// re-read whenever it is about to be used instead of trusting the boot-time
	// snapshot (#4889).
	programFollowsConfig bool
}

// newProgramChoice seeds the launch choice. An empty program means no --program
// flag was given, so the default follows config. The global value only seeds
// the cache: defaultProgram resolves the active project's own value at use.
func newProgramChoice(program string, appConfig *config.Config) programChoice {
	if program != "" {
		return programChoice{program: program}
	}
	return programChoice{program: appConfig.DefaultProgram, programFollowsConfig: true}
}

// defaultProgram returns the program a new session or task defaults to: the
// explicit launch choice when there is one, otherwise the default_program the
// active project resolves to right now — project override > global > built-in,
// the same resolution `af config get default_program` reports for that scope.
//
// The config is read at the moment of use (opening the new-session form,
// saving a task) rather than cached from launch, so a live `af config set
// default_program` reaches the running TUI without a restart. A read failure
// keeps the last-known value: the form still opens, and the create preflight
// re-resolves the config and names the problem on submit.
func (m *home) defaultProgram() string {
	if !m.programFollowsConfig {
		return m.program
	}
	if program, err := m.resolveConfiguredProgram(); err != nil {
		log.WarningLog.Printf("default program: could not re-read config, keeping %q: %v", m.program, err)
	} else if program != "" {
		m.program = program
	}
	return m.program
}

// resolveConfiguredProgram reads default_program for the active project, or
// from the global config alone when no project is active (registry mode).
func (m *home) resolveConfiguredProgram() (string, error) {
	if m.repoRoot == "" {
		cfg, err := config.LoadConfig()
		if err != nil {
			return "", err
		}
		return cfg.DefaultProgram, nil
	}
	repo, err := config.RepoFromPath(m.repoRoot)
	if err != nil {
		return "", err
	}
	resolved, err := config.ResolveConfigForRepo(repo)
	if err != nil {
		return "", err
	}
	return resolved.DefaultProgram, nil
}
