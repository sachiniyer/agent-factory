package config

import (
	"errors"
	"fmt"
)

// The config editors' project read (#7009). The `,` editor and the web Config
// view render ConfigEntry rows; a global read gives every row the global file's
// value, which for a key the project overrides is not what af uses there. This
// is the project-effective counterpart both surfaces share — the daemon serves
// it for the web and a remote TUI, the local TUI calls it in-process — so the
// precedence lives in the resolver `af config get --repo` already uses rather
// than in either UI.

// The values ConfigEntry.Scope carries. The set is OPEN on the wire: a client
// must treat a value it does not know as "a non-global layer supplied this",
// which is what keeps a future layer from breaking older clients.
const (
	// ScopeProject is a checked-in or legacy per-repo value, or a root_agent the
	// project resolves itself (an ignored global, or a fail-closed profile).
	ScopeProject = "project"
	// ScopePersonal is the machine-local per-project layer.
	ScopePersonal = "personal"
	// ScopeProjectPersonal is a composite whose leaves came from both.
	ScopeProjectPersonal = "project+personal"
	// ScopePersonalIgnored is a personal value that is stored but not applied
	// (branch_prefix until #4539): the effective value is still global, and the
	// marker exists so the stored value is not invisible.
	ScopePersonalIgnored = "personal-ignored"
)

// ErrProjectPathUnusable marks a project path the caller sent that cannot be
// resolved at all (relative, on a daemon that has no caller cwd). The daemon
// wraps its path-normalization failure with it so ProjectScopeError can say so
// without quoting that failure.
var ErrProjectPathUnusable = errors.New("unusable project path")

// EditorEntriesForRepo returns the manifest rows zipped with the project's
// EFFECTIVE values — the same resolution `af config get --repo` reports — plus a
// scope marker and the global value on every row a non-global layer supplies. It
// is the read the `,` editor and the web Config view share, so neither surface
// re-derives precedence.
//
// A row no project layer touches is the ManifestWithValues row verbatim, value
// form included: the resolver's rendering of a composite can differ from the
// editor form (an absent [account_handoff] resolves to its full default table),
// and an unmarked row promises it IS the global row — the edit baseline and an
// untouched save depend on that.
func EditorEntriesForRepo(repo *RepoContext, global *Config) (entries []ConfigEntry, projectRoot string, err error) {
	if global == nil {
		return nil, "", fmt.Errorf("global config snapshot is required for a project editor read")
	}
	// Non-strict like a session start: one unreadable registry record degrades
	// to "no personal layer" instead of hiding the whole project's values.
	resolved, err := ResolveConfigForRepoInspectionWithGlobalNonStrict(repo, global)
	if err != nil {
		return nil, "", err
	}
	values, err := RootAgentAwareResolution(resolved, repo.WorkspacePath(), false)
	if err != nil {
		return nil, "", err
	}
	byKey := make(map[string]ResolvedValue, len(values))
	for _, v := range values {
		byKey[v.Key] = v
	}
	entries = ManifestWithValues(global)
	for i := range entries {
		rv, ok := byKey[entries[i].Key]
		if !ok {
			continue
		}
		scope := editorEntryScope(rv)
		if scope == "" {
			continue
		}
		entries[i].GlobalValue = entries[i].Value
		entries[i].Value = FormatValueForKey(entries[i].Key, rv.Value)
		entries[i].Scope = scope
	}
	return entries, resolved.ProjectRoot, nil
}

// editorEntryScope classifies which non-global layers supplied rv, or "" when
// only the built-in and global layers did.
func editorEntryScope(rv ResolvedValue) string {
	if PersonalBranchPrefixIgnored(rv) {
		return ScopePersonalIgnored
	}
	// The project decided its own root_agent: the global table is dropped
	// (unregistered, no legacy entry) or the profile failed closed. Either way
	// the global value is not what applies here.
	if rv.Key == "root_agent" && (RootAgentValueFailsClosed(rv) || RootAgentValueHasIgnoredGlobal(rv)) {
		return ScopeProject
	}
	project, personal := false, false
	mark := func(layer string) {
		switch layer {
		case SourceRepoShared.String(), SourceLegacyRepo.String(), string(RootAgentSourceLegacy):
			project = true
		case SourceProjectPersonal.String():
			personal = true
		}
	}
	if rv.Winner != nil {
		mark(rv.Winner.Layer)
	}
	for _, origin := range rv.Origins {
		mark(origin.Layer)
	}
	switch {
	case project && personal:
		return ScopeProjectPersonal
	case project:
		return ScopeProject
	case personal:
		return ScopePersonal
	}
	return ""
}

// ProjectScopeError renders the actionable, remote-safe failure for a
// project-scoped config read: it names only the path the caller asked about
// and what to do next. It deliberately never embeds the resolver's error —
// that text can name daemon-host paths (registry, AF_HOME, personal config)
// the requester did not supply, and this string crosses the wire.
func ProjectScopeError(requested string, err error) string {
	switch {
	case errors.Is(err, ErrProjectPathUnusable):
		return "not a usable path: " + requested + " — send an absolute path on the daemon host (or one starting with ~/)"
	case errors.Is(err, ErrNotGitRepository):
		return "not a git repository: " + requested
	case RepoProbeUnanswered(err):
		return "could not check " + requested + " — git did not answer; the project may be on an unavailable mount, try again"
	}
	return "af could not read the project config for " + requested +
		"; run `af config get --repo " + requested + " default_program` on the daemon host for the exact error"
}
