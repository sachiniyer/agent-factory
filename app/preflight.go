package app

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session"
)

var (
	localSessionPreflight = preflight.LocalSessionPrereqs
	localPrereqsRequired  = session.LocalPrereqsRequired
)

func (m *home) preflightSessionCreate() error {
	// A remote target hosts the session on the daemon's machine — its tmux,
	// its PATH, its backend resolution, its default_program. Checked FIRST,
	// before LocalPrereqsRequired resolves the client's repo config: every
	// client-local question can both block the event loop on this host's
	// filesystem and refuse a create the remote could serve (an untouched
	// form sends "" for the daemon to resolve, so substituting the client's
	// cfg.DefaultProgram below is actively wrong when the hosts differ). The
	// daemon rechecks the program itself at create time — remote creates
	// skip client-local prereqs (#4889 review).
	if isRemoteTarget() {
		return nil
	}
	// A backend picked in the naming form's field (#1933) decides this, and the
	// placeholder instance cannot: it is constructed when the form OPENS, before the
	// user has chosen anything, so its capabilities describe the repo default rather
	// than the create about to be submitted. Checking the local agent binary for a
	// docker/ssh/hook create would refuse a session whose agent runs somewhere else
	// entirely — the shape of #2592 on the CLI side.
	//
	// Both surfaces now ask the same question through session.LocalPrereqsRequired
	// rather than each hand-rolling "is the picked backend local", so the
	// precedence between an explicit pick and the repo's `backend` key is decided
	// in exactly one place (#2592). Passing the pick RAW is what preserves that
	// precedence: an untouched field is "", which defers to the repo's key, while
	// an explicit `local` pick outranks it.
	//
	// The resolve error is deliberately not surfaced here, and the bool is still
	// exactly right when it arrives: unresolvable is NOT local. The picker offers
	// whatever the daemon's catalog lists, which may name a backend this process's
	// enum has never heard of (#2600's anti-drift property) — refusing it here
	// would substitute a client-side enum for the catalog the TUI deliberately
	// does not hold. `local` is the one name this side can always recognize, so a
	// name that does not resolve cannot be the local backend, and a missing local
	// `claude` is not what is wrong with it. The daemon owns the verdict on a
	// backend it could not resolve, and states it when the create is submitted.
	//
	local, _ := localPrereqsRequired(session.InstanceOptions{
		Backend: session.BackendKind(m.pendingBackend),
		Host:    m.pendingHost,
	}, m.repoRoot)
	if !local {
		return nil
	}
	cfg, err := m.preflightConfig()
	if err != nil {
		return err
	}
	// Preflight must check the program the wire carries — the same derivation
	// the submit path applies (#4889 review). An untouched form sends "" and
	// the daemon resolves the LIVE default_program at create time, so cfg's
	// default_program is the program to check; an explicit pick, a launch
	// --program, or a CHOSEN account instead pins the displayed
	// pendingProgram, and checking the config value then would refuse a valid
	// request or approve one whose sent program lacks local prerequisites.
	program := m.sendProgramForCreate()
	if program == "" {
		program = cfg.DefaultProgram
	}
	return localSessionPreflight(cfg, program)
}

func (m *home) preflightConfig() (*config.Config, error) {
	if m.repoRoot != "" {
		repo, err := config.RepoFromPath(m.repoRoot)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve project before starting session: %w", err)
		}
		resolved, err := config.ResolveConfigForRepo(repo)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve config before starting session: %w", err)
		}
		return &resolved.Config, nil
	}
	if m.appConfig != nil {
		return m.appConfig, nil
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config before starting session: %w", err)
	}
	return cfg, nil
}

func SetLocalSessionPreflightForTest(f func(*config.Config, string) error) func() {
	prev := localSessionPreflight
	localSessionPreflight = f
	return func() { localSessionPreflight = prev }
}
