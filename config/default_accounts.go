package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// The `default_accounts` key: which credential account a session runs as when the
// create names none (#3386).
//
// Validation is split deliberately, and the split is the design:
//
//   - HERE, at load and write time, the entry's SHAPE is checked — the key names
//     an agent whose sessions can be scoped to an account at all, and the value
//     is a well-formed account name. Both are facts about the text, so they hold
//     on any machine and they are what a hand-edited file is checked against.
//   - AT CREATE TIME, in the daemon, the account's EXISTENCE is checked, and a
//     default that cannot be honoured REFUSES the create naming this key. That is
//     the only place it can be checked honestly: the accounts live in the
//     daemon's home, which for a remote daemon is not this machine, and it is the
//     only place where falling back to the ambient identity would be the silent
//     wrong-identity outcome (#2983) rather than a note.
//
// The write path additionally WARNS when the named account is not registered
// here, which is the same fact reported at the moment a typo is made rather than
// at the next create. It is not an error: configuring a project before
// registering the account is a legitimate order, and the manifest's Enum contract
// (TestManifestEnumIsEnforcedAtSetTime) requires this validator to accept every
// enumerated agent paired with an arbitrary value.

// ValidateDefaultAccountEntry checks one `default_accounts` entry's shape. lead
// is the caller's context sentence ("Config issue in <path>", "default_accounts
// key"), so a loader failure names the file and a `config set` failure names the
// command.
func ValidateDefaultAccountEntry(lead, agent, name string) error {
	if _, ok := sessionenv.SupportsAccounts(agent); !ok {
		return fmt.Errorf(
			"%s: default_accounts.%s names an agent whose sessions cannot be scoped to an account, so the "+
				"default would never apply; account scoping supports %s",
			lead, agent, sessionenv.AccountAgentsSummary())
	}
	if strings.TrimSpace(name) == "" {
		// EMPTY IS A VALUE, not a mistake, and it is the only way to clear this key.
		// `af config unset` without --project clears three migrated backend settings
		// and refuses everything else, so an empty value is what removes a GLOBAL
		// default — exactly as it removes a program_overrides entry.
		//
		// In the personal per-project layer it is also the opt-out: the key merges per
		// agent, so a project entry of "" is present, wins for that agent, and means
		// "this project runs on the ambient identity" even when the global layer names
		// an account.
		return nil
	}
	if err := agentaccount.ValidateName(name); err != nil {
		return fmt.Errorf("%s: default_accounts.%s is not a usable account name: %w", lead, agent, err)
	}
	return nil
}

// validateDefaultAccounts runs ValidateDefaultAccountEntry over a whole map, in a
// stable key order so a file with two bad entries always reports the same one.
func validateDefaultAccounts(lead string, accounts map[string]string) error {
	agents := make([]string, 0, len(accounts))
	for agent := range accounts {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	for _, agent := range agents {
		if err := ValidateDefaultAccountEntry(lead, agent, accounts[agent]); err != nil {
			return err
		}
	}
	return nil
}

// unregisteredDefaultAccountWarning reports that a just-written default names an
// account that does not exist on THIS host yet, and names the command that makes
// it real. Empty when the account is registered, when the agent is not one that
// takes accounts (the validator already refused that), or when the registry
// cannot be read at all — a warning is a claim, and "af could not look" is not
// evidence that the account is missing.
func unregisteredDefaultAccountWarning(agent, name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	if _, ok := sessionenv.SupportsAccounts(agent); !ok {
		return ""
	}
	home, err := GetConfigDir()
	if err != nil {
		return ""
	}
	names, err := agentaccount.List(home, agent)
	if err != nil {
		return ""
	}
	for _, registered := range names {
		if registered == name {
			return ""
		}
	}
	warning := fmt.Sprintf(
		"WARNING: no %s account named %q is registered on this machine, so a session using this default will "+
			"be refused rather than started. Register it with `af accounts add %s %s` and log in",
		agent, name, agent, name)
	if len(names) > 0 {
		warning += fmt.Sprintf(" · registered %s accounts: %s", agent, strings.Join(names, ", "))
	}
	return warning + "."
}

// defaultAccountWriteWarning is the write path's hook: it returns the warning for
// a `default_accounts` write and "" for every other key, so both Set*ConfigValue
// paths can call it unconditionally.
func defaultAccountWriteWarning(key, leaf, value string) string {
	if key != "default_accounts."+leaf {
		return ""
	}
	return unregisteredDefaultAccountWarning(leaf, value)
}

// DefaultAccountSelection is a `default_accounts` entry as it was actually
// resolved: the account, the agent whose registry it lives in, and WHERE it was
// declared.
//
// The provenance is not decoration. A create that refuses because of this key
// refuses for a value the user did not type on that command — they set it once,
// possibly weeks ago, possibly in a different file from the one they are looking
// at. "account \"work\" is not registered" would send them hunting; naming the
// key, the layer and the file turns the refusal into an edit.
type DefaultAccountSelection struct {
	// Agent is the agent whose account registry Name belongs to.
	Agent string
	// Name is the account, or "" when this layer declared none.
	Name string
	// Layer is the config layer that declared it, for the "how to clear it" hint.
	Layer ConfigSource
	// Path is the file it was declared in, already prettified for a message.
	Path string
}

// Key is the fully-qualified config key this selection came from.
func (s DefaultAccountSelection) Key() string {
	return "default_accounts." + s.Agent
}

// Source is the phrase a message uses to name where this default came from.
func (s DefaultAccountSelection) Source() string {
	phrase := s.Key()
	if s.Layer != SourceInvalid {
		phrase += " in the " + s.Layer.String() + " config"
	}
	if s.Path != "" {
		phrase += " " + s.Path
	}
	return phrase
}

// ClearHint is the command that removes this default — and it is a command that
// RUNS, which is why the two layers get different ones.
//
// `af config unset` without --project clears three migrated backend settings and
// refuses everything else, so pointing a global default at it would print an
// instruction that errors. An empty value is what clears a global entry, the same
// way it clears a program_overrides one.
func (s DefaultAccountSelection) ClearHint(repoPath string) string {
	if s.Layer == SourceProjectPersonal && repoPath != "" {
		return fmt.Sprintf("af config unset %s --project %s", s.Key(), repoPath)
	}
	return fmt.Sprintf(`af config set %s ""`, s.Key())
}

// DefaultAccountLayersFor reports the `default_accounts` entry for agent from the
// two layers that may declare one, so a caller can run them through
// agentaccount.Resolve in its own precedence rather than being handed a
// pre-collapsed answer.
//
// The PROJECT selection is the per-repo resolution, which already folds the
// global layer under the personal one — so when the global entry is the one that
// wins there, the returned provenance says "global", and it says "personal
// project" when the project overrode it. The GLOBAL selection is returned ONLY as
// the fallback for a resolution that could not happen at all: no repo path, a path
// Git does not recognize, or a config that does not load. Falling back rather than
// failing matches defaultProgramFor — the create surfaces a real path problem a
// moment later, with more context than this read has — and the two are never
// returned together, so a project's empty opt-out cannot be resolved past.
//
// It reads through ResolveConfigForRepoInspection, never the recording variant:
// answering "what account would a create use" is a read, and a read must not
// create durable state (the catalog RPC behind the TUI and web pickers calls this
// on every form open).
func DefaultAccountLayersFor(global *Config, repoPath, agent string) (project, globalLayer DefaultAccountSelection) {
	if strings.TrimSpace(agent) == "" {
		return DefaultAccountSelection{}, DefaultAccountSelection{}
	}
	if global != nil {
		if name := strings.TrimSpace(global.DefaultAccounts[agent]); name != "" {
			path, err := globalConfigTomlPath()
			if err != nil {
				path = ""
			}
			globalLayer = DefaultAccountSelection{
				Agent: agent, Name: name, Layer: SourceGlobal, Path: prettyHomePath(path),
			}
		}
	}
	if strings.TrimSpace(repoPath) == "" {
		return DefaultAccountSelection{}, globalLayer
	}
	repo, err := RepoFromPath(repoPath)
	if err != nil {
		return DefaultAccountSelection{}, globalLayer
	}
	resolved, err := ResolveConfigForRepoInspection(repo)
	if err != nil {
		return DefaultAccountSelection{}, globalLayer
	}
	// From here the repo resolution is AUTHORITATIVE and the fallback is dropped.
	// It has already folded the global layer under the personal one — including a
	// project entry set to "" to opt out of a global default — so returning the
	// global selection beside it would let a caller resolve past that opt-out and
	// scope the session to the very account the project turned off.
	name := strings.TrimSpace(resolved.DefaultAccounts[agent])
	if name == "" {
		return DefaultAccountSelection{}, DefaultAccountSelection{}
	}
	// The pre-trace attribution, used only if the trace below cannot be read. It is
	// derived rather than assumed: a value that differs from the global snapshot —
	// or is absent from it — did not come from the global layer, and guessing
	// "global" there would print `af config set … ""`, a command that leaves the
	// project's entry exactly where it is.
	project = DefaultAccountSelection{Agent: agent, Name: name, Layer: SourceProjectPersonal}
	if globalLayer.Name == name {
		project.Layer, project.Path = SourceGlobal, globalLayer.Path
	}
	// The manifest resolves this key per agent (MergeMapByKey), so the trace
	// attributes each entry on its own — which is exactly the granularity a
	// message needs. A trace that cannot be read leaves the layer as the global
	// default above rather than inventing one.
	if value, ok := resolved.ResolvedValue("default_accounts"); ok {
		if origin, found := value.Origins[agent]; found {
			project.Layer = sourceForLayerName(origin.Layer)
			if origin.Path != "" {
				project.Path = prettyHomePath(origin.Path)
			}
		}
	}
	return project, DefaultAccountSelection{}
}

// ResolvedDefaultAccountsFor reports the effective `default_accounts` map for
// repoPath — every agent's configured default, with the personal per-project
// layer merged over the global one exactly as a create resolves it.
//
// It exists beside DefaultAccountLayersFor because the two answer different
// questions at different costs. A create needs ONE agent's value plus the
// provenance a refusal will name; a catalog needs EVERY agent's value and no
// provenance at all, and calling the per-agent function in a loop would resolve
// the repository's whole config once per agent — several git probes per open of a
// form that has not been submitted yet.
//
// A repo that cannot be resolved falls back to the global map, which is the same
// fallback the create applies, so the picker and the create agree there too.
func ResolvedDefaultAccountsFor(global *Config, repoPath string) map[string]string {
	fallback := map[string]string{}
	if global != nil {
		for agent, name := range global.DefaultAccounts {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				fallback[agent] = trimmed
			}
		}
	}
	if strings.TrimSpace(repoPath) == "" {
		return fallback
	}
	repo, err := RepoFromPath(repoPath)
	if err != nil {
		return fallback
	}
	resolved, err := ResolveConfigForRepoInspection(repo)
	if err != nil {
		return fallback
	}
	effective := map[string]string{}
	for agent, name := range resolved.DefaultAccounts {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			effective[agent] = trimmed
		}
	}
	return effective
}

// sourceForLayerName maps a trace's layer string back to its ConfigSource. The
// trace carries the presentation form, and ClearHint needs the identity.
func sourceForLayerName(layer string) ConfigSource {
	for source := SourceBuiltIn; source < configSourceCount; source++ {
		if source.String() == layer {
			return source
		}
	}
	return SourceInvalid
}

// CheckDefaultAccount reports why a config-declared default cannot scope a
// session, or nil when it can. home is the agent-factory home whose accounts
// directory holds the registries — the DAEMON's home, since that is where a
// session's credentials are read from.
//
// It refuses rather than falling back to the ambient identity, and that is the
// whole point of the key having a checker at all: a session that quietly ran as
// somebody else while the config said otherwise is the failure this feature
// exists to remove (#2983). repoPath only sharpens the "how to clear it" hint.
func CheckDefaultAccount(home, repoPath string, selection DefaultAccountSelection) error {
	if strings.TrimSpace(selection.Name) == "" {
		return nil
	}
	if reason, ok := sessionenv.AccountRegistrationOnlyReason(selection.Agent); ok {
		return fmt.Errorf("%s selects account %q, but %s. Clear the default with `%s`",
			selection.Source(), selection.Name, reason, selection.ClearHint(repoPath))
	}
	if _, err := agentaccount.Selected(home, selection.Agent, selection.Name, ""); err != nil {
		return fmt.Errorf(
			"%s selects account %q, and the session was NOT created: %w. Register it, or clear the default "+
				"with `%s` to run this project's %s sessions on the ambient identity",
			selection.Source(), selection.Name, err, selection.ClearHint(repoPath), selection.Agent)
	}
	return nil
}
