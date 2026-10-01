package config

import (
	"fmt"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// A project-scoped branch_prefix is accepted but deliberately not applied
// (#4539). Session branch names come from the global prefix the daemon froze
// at startup, so the personal layer is kept as a WRITE location — existing
// personal config files stay valid and `af config set --project branch_prefix`
// still stores — while the key's precedence is global-only so the stored value
// can never win. This file owns the three places that interim state is said
// out loud: the warning `af config set --project` prints, the memoized warning
// a config load emits, and the trace annotation/get-list marker the inspection
// commands render.

// projectBranchPrefixWarningFor is the canonical sentence for the interim
// state, naming the global prefix that actually applies.
func projectBranchPrefixWarningFor(globalPrefix string) string {
	return fmt.Sprintf("branch_prefix is not supported per project yet; the global branch_prefix (%s) applies to all projects. See #4539.",
		globalPrefix)
}

// projectBranchPrefixWarning renders that sentence for a caller that does not
// already hold the resolved value.
func projectBranchPrefixWarning() string {
	return projectBranchPrefixWarningFor(globalBranchPrefixValue())
}

// globalBranchPrefixValue reports the effective global branch_prefix the
// warning names via a strictly read-only load — the same file `af config get`
// resolves, falling back to the built-in when it cannot be read, so a warning
// never mutates the AF home (no materialization, no migration) and never fails
// with the file.
func globalBranchPrefixValue() string {
	loaded, err := LoadConfigReadOnly()
	if err == nil && loaded.Config != nil {
		return loaded.Config.BranchPrefix
	}
	return DefaultConfig().BranchPrefix
}

// projectBranchPrefixWarned memoizes the load warning to one emission per
// personal config file per process — the daemon and TUI reload config on their
// own cadences, so an unbounded warning would repeat every cycle. The key is
// the file path so two different projects warn independently (the pattern
// removedPRKeysWarned uses).
var projectBranchPrefixWarned sync.Map

// warnProjectBranchPrefixIgnored announces that a personal project config
// declares branch_prefix, a value stored but not applied (#4539), naming the
// effective prefix THIS resolution already computed — callers never re-load
// the global file to say it, so even read-only inspection cannot mutate the
// AF home. It fires at most once per file per process and reaches both the log
// and, when a command wired it, interactive stderr.
func warnProjectBranchPrefixIgnored(path, effectiveGlobalPrefix string) {
	key := path
	if key == "" {
		key = "(unknown)"
	}
	if _, seen := projectBranchPrefixWarned.LoadOrStore(key, struct{}{}); seen {
		return
	}
	msg := fmt.Sprintf("personal project config %s: %s",
		prettyHomePath(path), projectBranchPrefixWarningFor(effectiveGlobalPrefix))
	log.WarningLog.Print(msg)
	writeInteractiveWarning(msg)
}

// resetProjectBranchPrefixWarnings clears the per-file memo so a test can
// observe the warning for a source an earlier test already warned about —
// registered beside the other once-per-source resets in captureLog.
func resetProjectBranchPrefixWarnings() {
	projectBranchPrefixWarned.Range(func(key, _ any) bool {
		projectBranchPrefixWarned.Delete(key)
		return true
	})
}

// projectBranchPrefixWriteWarning is the set path's hook, mirroring
// defaultAccountWriteWarning: the write still lands (existing configs stay
// valid), so this is a SetResult warning rather than an error.
func projectBranchPrefixWriteWarning(key string) string {
	if canonicalConfigKey(key) != "branch_prefix" {
		return ""
	}
	return projectBranchPrefixWarning()
}

// annotateProjectBranchPrefix relabels a present personal-project candidate on
// a resolved branch_prefix: it was excluded by precedence, but the generic
// "disallowed" reads like the location is not supported at all — it is: the
// value is accepted and stored, just not applied (#4539). It also emits the
// once-per-file load warning here — the resolve is where every consumer's
// config load funnels, and res.BranchPrefix is already the effective prefix
// THIS resolution computed, so the warning names it without touching the
// global file again. No-op when no personal layer participated (the
// global-only resolve).
func annotateProjectBranchPrefix(res *ResolvedConfig) {
	for i := range res.Resolution {
		value := &res.Resolution[i]
		if value.Key != "branch_prefix" {
			continue
		}
		for j := range value.Candidates {
			candidate := &value.Candidates[j]
			if candidate.Layer == SourceProjectPersonal.String() && candidate.Present && !candidate.Allowed {
				candidate.Result = "ignored"
				candidate.Reason = "accepted and stored, but not applied: " + projectBranchPrefixWarningFor(res.BranchPrefix)
				warnProjectBranchPrefixIgnored(candidate.Path, res.BranchPrefix)
			}
		}
	}
}

// PersonalBranchPrefixIgnored reports whether a resolved branch_prefix carries
// a personal per-project value that was stored but not applied (#4539), so
// `af config get/list` can mark the effective global value it shows instead.
func PersonalBranchPrefixIgnored(value ResolvedValue) bool {
	if value.Key != "branch_prefix" {
		return false
	}
	for _, candidate := range value.Candidates {
		if candidate.Layer == SourceProjectPersonal.String() && candidate.Present && !candidate.Allowed {
			return true
		}
	}
	return false
}
