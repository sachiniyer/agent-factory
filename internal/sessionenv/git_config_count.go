package sessionenv

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file holds the GIT_CONFIG_COUNT completeness helpers shared by the
// boundary (session-creation validation) and the exec shims (defense-in-depth
// neutralization). GIT_CONFIG_COUNT is whole: git treats the count as
// authoritative and fails fatally ("error: missing config key GIT_CONFIG_KEY_0")
// when the count names pairs that are not present, so af refuses a count-only
// forward whose value resolves to N>0 without GIT_CONFIG_KEY_0..N-1 /
// GIT_CONFIG_VALUE_0..N-1, and — where the forward bypassed the create refusal
// — pins the count defined-empty at the exec shim so git reads 0 and does not
// fail. strtoul("") is 0 with no error, so an empty count makes every indexed
// GIT_CONFIG_KEY_* / GIT_CONFIG_VALUE_* pair inert — the same shape an
// unforwarded count takes.

// parseGitConfigCount parses a GIT_CONFIG_COUNT value the way git's strtoul
// does: leading whitespace is skipped, an optional single leading '+' is
// consumed, the following run of base-10 digits is the count, and an empty or
// non-numeric-leading value yields 0. A count of 0 makes every indexed
// GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* pair inert, so no pairs are required.
// Git itself rejects a negative count, and a host value of that shape is
// already broken regardless of forwarding, so only a leading '+' (which
// strtoul accepts) plus non-negative digits are recognized — not '-'.
func parseGitConfigCount(value string) int {
	value = strings.TrimLeftFunc(value, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
	})
	value = strings.TrimPrefix(value, "+")
	n := 0
	seen := false
	for _, r := range value {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		seen = true
	}
	if !seen {
		return 0
	}
	return n
}

// gitConfigCountMaxExpand bounds the work the completeness helpers do for a
// forwarded count whose pairs did not travel with it. A count larger than this
// cannot be a complete forward through session_env_passthrough (it would name
// more indexed pairs than any operator forwards), and enumerating every
// required index would allocate and retain a formatted string per index before
// the docker account session can be neutralized — e.g. GIT_CONFIG_COUNT=10000000
// with only the count forwarded. Such a count is treated as incomplete (refused
// at the boundary, neutralized at the shims) and the refusal names the bounded
// range instead of materializing every index. The limit is far above any
// realistic GIT_CONFIG_COUNT while keeping the per-call work constant-bounded.
const gitConfigCountMaxExpand = 4096

// envEntryValue reports whether name is present in environ (defined, even
// empty) and returns its value. It is the getenv over a name=value slice the
// boundary functions share with the production filter.
func envEntryValue(environ []string, name string) (string, bool) {
	for _, entry := range environ {
		if n, v, ok := strings.Cut(entry, "="); ok && n == name {
			return v, true
		}
	}
	return "", false
}

// forceEnvEntryEmpty returns environ with name defined-and-empty, replacing
// any existing assignment for it exactly once. A defined-empty count is what
// renders the whole unbounded GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* family
// inert, which is the neutralized shape this installs.
func forceEnvEntryEmpty(environ []string, name string) []string {
	out := make([]string, 0, len(environ))
	done := false
	empty := name + "="
	for _, entry := range environ {
		if n, _, ok := strings.Cut(entry, "="); ok && n == name {
			if !done {
				out = append(out, empty)
				done = true
			}
			continue
		}
		out = append(out, entry)
	}
	if !done {
		out = append(out, empty)
	}
	return out
}

// gitConfigCountMissingPairs reports whether a pass-through list forwards
// GIT_CONFIG_COUNT with a live value resolving to N>0 in source WITHOUT also
// forwarding every required indexed pair GIT_CONFIG_KEY_0..N-1 and
// GIT_CONFIG_VALUE_0..N-1 AND those pairs resolving to usable values in
// source. When it does, missing is the sorted list of pair names the operator
// still needs to forward (or whose source value is missing/empty); otherwise
// missing is nil. A count that is not forwarded, absent in source, empty, 0,
// or non-numeric is a complete forward — no pairs are required.
//
// This is the shape test against the OPERATOR'S LIST and the value's source:
// it answers "did the operator forward a count whose pairs did not travel
// with it", which is what refuse-loudly and the docker blanking both need.
// Completeness accounts for the resolved source entries, not just the names
// in the pass-through list: a pair name the operator forwarded but whose
// source value is absent (keep-if-set and docker's bare -e both read source)
// arrives defined-empty or absent in the delivered environment, and git fails
// a missing OR empty GIT_CONFIG_KEY_i ("empty config key") — so a forwarded
// KEY whose source value is missing or empty is the same half-forward. An
// empty GIT_CONFIG_VALUE_i is a valid git config value, so only its absence is
// incomplete. Only pair NAMES ever enter missing — the values themselves are
// read for the empty/absent test and discarded, so a malformed NAME=value
// entry cannot render a credential in a caller's error.
func gitConfigCountMissingPairs(passthrough, source []string) (count int, missing []string) {
	wanted := make(map[string]struct{}, len(passthrough))
	for _, name := range passthrough {
		wanted[strings.TrimSpace(name)] = struct{}{}
	}
	if _, ok := wanted["GIT_CONFIG_COUNT"]; !ok {
		return 0, nil
	}
	value, present := envEntryValue(source, "GIT_CONFIG_COUNT")
	if !present {
		return 0, nil
	}
	count = parseGitConfigCount(value)
	if count <= 0 {
		return count, nil
	}
	// Bound the expansion: a count this large is not a complete forward and
	// enumerating every index would allocate unbounded memory. Name the
	// bounded range so the refusal is honest about not listing every index.
	if count > gitConfigCountMaxExpand {
		missing = []string{
			fmt.Sprintf("GIT_CONFIG_KEY_0..GIT_CONFIG_KEY_%d", count-1),
			fmt.Sprintf("GIT_CONFIG_VALUE_0..GIT_CONFIG_VALUE_%d", count-1),
		}
		sort.Strings(missing)
		return count, missing
	}
	for i := 0; i < count; i++ {
		keyName := "GIT_CONFIG_KEY_" + strconv.Itoa(i)
		valName := "GIT_CONFIG_VALUE_" + strconv.Itoa(i)
		if _, ok := wanted[keyName]; !ok {
			missing = append(missing, keyName)
		} else if kv, kpresent := envEntryValue(source, keyName); !kpresent || kv == "" {
			// Forwarded by name but unresolved or empty in source: the
			// delivered pair is defined-empty (docker account blanking) or
			// absent (keep-if-set reads source), and git fails an empty
			// GIT_CONFIG_KEY_i just as it fails a missing one.
			missing = append(missing, keyName)
		}
		if _, ok := wanted[valName]; !ok {
			missing = append(missing, valName)
		} else if _, vpresent := envEntryValue(source, valName); !vpresent {
			// Forwarded by name but absent in source: the value does not
			// travel into the delivered environment, so git does not see the
			// pair. An empty value is a valid git config value, so only
			// absence is incomplete.
			missing = append(missing, valName)
		}
	}
	sort.Strings(missing)
	return count, missing
}

// GitConfigCountForwardMissingPairs returns the sorted indexed pair names a
// pass-through list is missing for a forwarded GIT_CONFIG_COUNT whose value
// resolves to N>0 in source — GIT_CONFIG_KEY_0..N-1 and
// GIT_CONFIG_VALUE_0..N-1 the operator did not also forward. It returns nil
// when the forward is complete (the count is not forwarded or resolves to 0
// or is absent, or every required pair is also forwarded). Provisioners use
// it to neutralize a count-only forward that the session-creation refusal
// missed, and to warn before doing so.
func GitConfigCountForwardMissingPairs(passthrough, source []string) []string {
	_, missing := gitConfigCountMissingPairs(passthrough, source)
	return missing
}

// ValidateGitConfigCountForward refuses a pass-through list that forwards
// GIT_CONFIG_COUNT with a live value of N>0 in source without also forwarding
// every required indexed pair GIT_CONFIG_KEY_0..N-1 and
// GIT_CONFIG_VALUE_0..N-1. Such a half-forward delivers a count git treats as
// authoritative alongside zero pairs, which makes every git invocation fail
// fatally (git >=2.31: "missing config key GIT_CONFIG_KEY_0"). af's stated
// posture is to refuse loudly; the remedy is to also forward the named
// missing pair names through the same pass-through list.
//
// source is the environment the count value resolves against — the daemon's
// environment for a docker session (where a bare -e GIT_CONFIG_COUNT reads
// it) or os.Environ for a local/ssh session (where the exec filter's
// keep-if-set keeps it). The check names only indices, never values, so a
// malformed NAME=value entry cannot render a credential in the error.
func ValidateGitConfigCountForward(passthrough, source []string) error {
	count, missing := gitConfigCountMissingPairs(passthrough, source)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("session_env_passthrough forwards GIT_CONFIG_COUNT (resolves to %d in the environment) without its required indexed pairs; git treats the count as authoritative and fails fatally when the pairs are absent — also forward %s through session_env_passthrough",
		count, strings.Join(missing, ", "))
}

// NeutralizeIncompleteGitConfigCount returns environ with GIT_CONFIG_COUNT
// pinned defined-empty when its value resolves to a positive count whose
// required indexed pairs are not all present in environ. It also reports
// whether it neutralized, so callers can surface the operator-facing
// diagnostic the silent-blank would otherwise hide.
//
// This is the defense-in-depth counterpart to ValidateGitConfigCountForward:
// it makes the delivered environment safe even when a forward bypassed the
// session-creation refusal — an older af daemon, an in-flight
// SetEnvPassthrough, or an environment whose count gained a value after
// admission. Presence is read from the FINAL environment (the one about to
// reach the agent or git): a required pair absent here is absent because the
// operator did not forward it or forwarded it without a value, and git fails
// either way, so the count is pinned empty (which strtoul reads as 0, making
// the whole family inert) rather than delivered live. A complete forward —
// count plus every required pair present — is left untouched.
func NeutralizeIncompleteGitConfigCount(environ []string) ([]string, bool) {
	value, ok := envEntryValue(environ, "GIT_CONFIG_COUNT")
	if !ok {
		return environ, false
	}
	count := parseGitConfigCount(value)
	if count <= 0 {
		return environ, false
	}
	// Bound the expansion: a count this large cannot be verified pair-by-pair
	// without unbounded work, and cannot be a complete forward through
	// session_env_passthrough, so pin it empty (the safe shape) the way the
	// bounded loop below would on the first missing pair.
	if count > gitConfigCountMaxExpand {
		return forceEnvEntryEmpty(environ, "GIT_CONFIG_COUNT"), true
	}
	for i := 0; i < count; i++ {
		if _, hasKey := envEntryValue(environ, "GIT_CONFIG_KEY_"+strconv.Itoa(i)); !hasKey {
			return forceEnvEntryEmpty(environ, "GIT_CONFIG_COUNT"), true
		}
		if _, hasVal := envEntryValue(environ, "GIT_CONFIG_VALUE_"+strconv.Itoa(i)); !hasVal {
			return forceEnvEntryEmpty(environ, "GIT_CONFIG_COUNT"), true
		}
	}
	return environ, false
}
