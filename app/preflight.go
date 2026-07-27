package app

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session"
)

var localSessionPreflight = preflight.LocalSessionPrereqs

func (m *home) preflightSessionCreate() error {
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
	// Every input is now a fact about the CREATE rather than about the placeholder
	// (#2599): the picked backend, and `N` via m.pendingForceRemote. The placeholder
	// used to answer the `N` half through its capabilities, which only worked
	// because it had been provisioned as a hook runtime while the user was still
	// typing a title. It is pinned local now and provisions nothing, so reading its
	// capabilities would report every create local and refuse a docker/ssh/hook
	// session for a missing local `claude` — #2592, reintroduced in the TUI.
	local, _ := session.LocalPrereqsRequired(session.InstanceOptions{
		Backend:     session.BackendKind(m.pendingBackend),
		ForceRemote: m.pendingForceRemote,
	}, m.repoRoot)
	if !local {
		return nil
	}
	cfg, err := m.preflightConfig()
	if err != nil {
		return err
	}
	return localSessionPreflight(cfg, m.pendingProgram)
}

func (m *home) preflightConfig() (*config.Config, error) {
	if m.repoRoot != "" {
		resolved, err := config.ResolveConfig(m.repoRoot)
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
