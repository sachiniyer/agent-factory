package config

import "fmt"

// This file is the single register of deprecated global-config spellings, and
// the reason `af config migrate` exists (#3624).
//
// Before it, a deprecation was detected in two unrelated places — the alias
// table in config_aliases.go for the flat network/ssh/docker/sandbox keys, and
// a hand-rolled check in rootagent_migration.go for root_agents — and each of
// them only knew how to WARN. The warnings fired on every config load (220 of
// each in eight days of one daemon log), every one of them ending in "no file
// was rewritten" with no command that would rewrite it: a notice the reader
// could not act on, repeated forever, inherited by every external user with an
// older config on upgrade.
//
// configDeprecations is now the one table both halves read. The warning's
// remedy clause and the migration's plan come from the SAME entry, so they
// cannot describe different remedies, and a deprecation added here cannot
// acquire a warning without also declaring what ends it. The type has no third
// state: an entry either carries a mechanical in-file rewrite (alias) or the
// manual step that stands in for one (manual), and
// TestConfigDeprecationsDeclareExactlyOneRemedy fails on an entry declaring
// neither or both.
//
// What this file does NOT do is retire a reader. Every deprecated spelling stays
// readable forever — an older config must keep loading, and a rolled-back binary
// must keep understanding a file this migration has rewritten. Migration is
// about ending the noise, not the compatibility.

// configDeprecation is one deprecated top-level config key together with the
// remedy that ends its warning.
type configDeprecation struct {
	// key is the deprecated spelling as it appears at the top level of the file.
	key string

	// alias, when non-nil, is the mechanical remedy: this key has a current
	// spelling in the SAME file, so `af config migrate` moves the value there
	// and deletes the flat line. Mutually exclusive with manual.
	alias *configKeyAlias

	// manual, when non-empty, is the remedy for a key with no in-file current
	// spelling: migrating it would need state outside the config file, so
	// `af config migrate` reports the key and leaves it exactly where it is.
	// The sentence names the step the reader must take instead. Mutually
	// exclusive with alias.
	manual string

	// present reports whether a decoded config carries this deprecation, and
	// names the specific instances the manual step applies to. It is REQUIRED
	// on a manual entry and nil on a rewrite entry, whose presence is simply
	// the flat key in the shape.
	//
	// It lives in the table rather than in a switch inside the migration for
	// the same reason the remedy does: a manual entry whose detection is
	// written somewhere else can warn while the migration silently never
	// reports it. TestConfigDeprecationsDeclareExactlyOneRemedy fails on a
	// manual entry that leaves this nil.
	present func(shape map[string]any) (detail []string, ok bool)
}

// legacyRootAgentsManualStep is why root_agents has no mechanical migration.
// Its successor is a REGISTERED project's personal [root_agent] file, so the
// rewrite would have to invent project registrations — durable state outside
// the config file, and visible in the TUI's project list. A config migration
// does not get to make that decision on the reader's behalf, so migrate names
// the step and stops.
//
// The step ends with removing the legacy entry on purpose. Registering the
// project and writing its [root_agent] reproduces the behavior, but the
// root_agents key is what the loader warns about — a remedy that stops before
// the removal leaves the reader having done the work and still seeing the
// warning that sent them (#3624 review).
const legacyRootAgentsManualStep = "register the path as a project, set enabled = true plus the optional program in its personal [root_agent], then remove its root_agents entry"

// configDeprecations returns every deprecated global-config key in one list.
// The flat aliases are derived from configKeyAliases rather than restated, so a
// new alias is a deprecation — with a migration — the moment it is added there.
func configDeprecations() []configDeprecation {
	out := make([]configDeprecation, 0, len(configKeyAliases)+1)
	for i := range configKeyAliases {
		out = append(out, configDeprecation{key: configKeyAliases[i].legacy, alias: &configKeyAliases[i]})
	}
	out = append(out, configDeprecation{
		key:     LegacyRootAgentsKey,
		manual:  legacyRootAgentsManualStep,
		present: legacyRootAgentsPaths,
	})
	return out
}

// LegacyRootAgentsKey is the legacy path-keyed root-agent map's config key.
const LegacyRootAgentsKey = "root_agents"

// migratable reports whether `af config migrate` can rewrite this key in place.
func (d configDeprecation) migratable() bool { return d.alias != nil }

// tomlRemedy is the clause every TOML deprecation warning ends with. It is the
// half of the warning that tells the reader how to stop seeing it, and it is
// generated here so it can never promise something the migration does not do.
func (d configDeprecation) tomlRemedy(bothPresent bool) string {
	if !d.migratable() {
		return "`af config migrate` rewrites the other deprecated keys but leaves this one in place, since it cannot register a project for you"
	}
	if bothPresent {
		return "run `af config migrate` to drop the flat spelling once the two agree"
	}
	return "run `af config migrate` to rewrite it in place"
}

// migrationRemedy is the same clause phrased for `af config migrate`'s own
// report, where the verb is already the thing being run.
func (d configDeprecation) migrationRemedy() string {
	if d.migratable() {
		return fmt.Sprintf("use %q", d.alias.canonical)
	}
	return d.manual
}
