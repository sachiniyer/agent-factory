package sessionenv

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// This file holds the GIT_CONFIG_COUNT completeness helpers shared by the
// exec shims (internal/sessionenv/exec_unix.go) and the docker account
// blanking (session/agent_credentials_env_loader.go). GIT_CONFIG_COUNT is
// whole: git treats the count as authoritative and fails fatally ("error:
// missing config key GIT_CONFIG_KEY_0") when the count names indexed pairs
// that are not present, so when a live count arrives without its required
// pairs af pins it defined-empty — strtoul("") is 0 with no error, which
// makes every indexed GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* pair inert, the
// same shape an unforwarded count takes — and warns naming the missing
// indices. A complete forward (count plus every required pair present) is
// untouched.
//
// The check runs where the environment is handed to the session, because
// that is the only place the count's value is known: on local/ssh it lives
// in the tmux server's environment at pane spawn, on docker in the
// daemon's environment the bare -e resolves against — neither is
// observable where the session or its pass-through list is created, so a
// create-time verdict could only guess.

// missingPairReportCap bounds the pair names a diagnostic lists for one
// broken forward: a valid count can reach INT_MAX, so the report names the
// first missing indices in order plus the exact total rather than
// materializing all 2N names.
const missingPairReportCap = 8

// parseGitConfigCount parses a GIT_CONFIG_COUNT value with git's own
// grammar — config.c's strtoul(env, &endp, 10), after which git dies on
// errno == ERANGE or count > INT_MAX ("too many entries in
// GIT_CONFIG_COUNT") and on a nonempty *endp ("bogus count in
// GIT_CONFIG_COUNT"). It returns (n, true) for a value git accepts as
// count n — "" and "-0" are the 0 git reads — and (0, false) for a value
// git itself rejects, which callers treat as not-a-count: the environment
// is left to git's own diagnostic, which names the malformed value rather
// than any index af guessed.
func parseGitConfigCount(value string) (int, bool) {
	if value == "" {
		return 0, true // strtoul("") is 0 with *endp at NUL: git reads count 0
	}
	s := strings.TrimLeft(value, " \t\n\v\f\r")
	neg := false
	if rest, ok := strings.CutPrefix(s, "+"); ok {
		s = rest
	} else if rest, ok := strings.CutPrefix(s, "-"); ok {
		neg = true
		s = rest
	}
	if s == "" {
		// All whitespace or a lone sign: strtoul converts nothing and leaves
		// *endp at the first byte — git dies "bogus count".
		return 0, false
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			// Trailing garbage: strtoul stops here, *endp != 0 — git dies
			// "bogus count".
			return 0, false
		}
		n = n*10 + uint64(c-'0')
		if n > math.MaxInt32 {
			// strtoul ERANGE or >INT_MAX — git dies "too many entries".
			return 0, false
		}
	}
	if neg && n != 0 {
		// strtoul negates modulo ULONG_MAX+1, so "-k" is a huge count for
		// any k>0 — git dies "too many entries". "-0" is a plain 0.
		return 0, false
	}
	return int(n), true
}

// gitConfigPairIndex parses the numeric suffix of a GIT_CONFIG_KEY_i /
// GIT_CONFIG_VALUE_i name and reports whether the name is index i exactly
// — git asks for the literal spelling, so "GIT_CONFIG_KEY_00" is not
// index 0 and a sign or trailing byte is not an index at all.
func gitConfigPairIndex(name, prefix string) (int, bool) {
	s, ok := strings.CutPrefix(name, prefix)
	if !ok || s == "" {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
		if n > math.MaxInt32 {
			return 0, false
		}
	}
	idx := int(n)
	if name != prefix+strconv.Itoa(idx) {
		return 0, false
	}
	return idx, true
}

// gitPairPresence records, per index i, whether the pair members a live
// count requires are usable: key[i] means GIT_CONFIG_KEY_i will be defined
// and non-empty for git (git fails an empty key — "empty config key" —
// exactly as it fails a missing one), value[i] means GIT_CONFIG_VALUE_i
// will be defined at all (an empty value is a legal empty config value).
type gitPairPresence struct {
	key   map[int]bool
	value map[int]bool
}

// missing reports the exact number of required pair names absent under p
// for a live count of N, and up to cap of their names in index order. The
// totals come from the presence sets and the listing stops at cap, so the
// cost tracks how many pairs are present, never the count value.
func (p gitPairPresence) missing(count, cap int) (names []string, total int) {
	presentKeys, presentValues := 0, 0
	for i, ok := range p.key {
		if ok && i < count {
			presentKeys++
		}
	}
	for i, ok := range p.value {
		if ok && i < count {
			presentValues++
		}
	}
	total = count - presentKeys + count - presentValues
	if total <= 0 {
		return nil, 0
	}
	for i := 0; i < count && len(names) < cap; i++ {
		if !p.key[i] {
			names = append(names, "GIT_CONFIG_KEY_"+strconv.Itoa(i))
		}
		if len(names) >= cap {
			break
		}
		if !p.value[i] {
			names = append(names, "GIT_CONFIG_VALUE_"+strconv.Itoa(i))
		}
	}
	return names, total
}

// gitPairPresenceInEnv indexes the GIT_CONFIG_KEY_i / GIT_CONFIG_VALUE_i
// entries an environment carries, first occurrence winning (getenv
// semantics): a defined-empty KEY counts as unusable — git fails it with
// "empty config key" — and a defined-empty VALUE counts as present.
func gitPairPresenceInEnv(environ []string) gitPairPresence {
	p := gitPairPresence{key: map[int]bool{}, value: map[int]bool{}}
	for _, entry := range environ {
		name, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if i, ok := gitConfigPairIndex(name, "GIT_CONFIG_KEY_"); ok {
			if _, seen := p.key[i]; !seen {
				p.key[i] = v != ""
			}
			continue
		}
		if i, ok := gitConfigPairIndex(name, "GIT_CONFIG_VALUE_"); ok {
			if _, seen := p.value[i]; !seen {
				p.value[i] = true
			}
		}
	}
	return p
}

// gitPairPresenceForwarded indexes the pair names a pass-through list
// forwards and reports what the forward DELIVERS for a docker account
// session: a forwarded pair arrives with the source's value when the
// source defines it — a defined-empty source value delivers a
// defined-empty pair, which is unusable for a KEY but legal for a VALUE —
// and when the source leaves it unset, arrives defined-and-empty only
// under an agent whose loader blanking emits NAME= for unset pair names
// (accountLoaderBlankEntries; gemini), staying absent under every other
// agent. wanted is the operator's normalized name set, built once by the
// caller.
func gitPairPresenceForwarded(agent string, wanted map[string]struct{}, source []string) gitPairPresence {
	pairsBlanked := len(AccountLoaderFilledNames(agent)) > 0
	values := make(map[string]string, len(source))
	for _, entry := range source {
		if n, v, ok := strings.Cut(entry, "="); ok {
			if _, seen := values[n]; !seen {
				values[n] = v
			}
		}
	}
	p := gitPairPresence{key: map[int]bool{}, value: map[int]bool{}}
	for name := range wanted {
		v, set := values[name]
		if i, ok := gitConfigPairIndex(name, "GIT_CONFIG_KEY_"); ok {
			p.key[i] = set && v != ""
			continue
		}
		if i, ok := gitConfigPairIndex(name, "GIT_CONFIG_VALUE_"); ok {
			p.value[i] = set || pairsBlanked
		}
	}
	return p
}

// gitConfigCountInEnv resolves the count an environment carries to a live
// positive count git accepts — (N, true) — and reports false when the name
// is absent, reads as 0, or holds a value git itself rejects (no pairs are
// required; that failure is git's to name, not af's to guess at).
func gitConfigCountInEnv(environ []string) (int, bool) {
	value, ok := envEntryValue(environ, "GIT_CONFIG_COUNT")
	if !ok {
		return 0, false
	}
	count, valid := parseGitConfigCount(value)
	if !valid || count <= 0 {
		return 0, false
	}
	return count, true
}

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

// GitConfigCountForwardMissingPairs reports whether a pass-through list
// forwards GIT_CONFIG_COUNT with a live value of N>0 in source WITHOUT the
// forward's delivery also carrying every required indexed pair
// GIT_CONFIG_KEY_0..N-1 / GIT_CONFIG_VALUE_0..N-1 — counting a pair that
// would arrive defined-but-empty as missing for a KEY (git fails "empty
// config key" exactly as it fails "missing config key"). names holds up to
// missingPairReportCap missing pair names in index order and total the
// exact missing count, so a diagnostic stays bounded no matter how large N
// is; total is 0 for a complete forward — the count is not forwarded, is
// absent or 0 in source, holds a value git itself rejects, or every
// required pair travels with it. agent decides what a forwarded-but-unset
// pair name delivers (see gitPairPresenceForwarded).
//
// Provisioners use this to neutralize a count-only forward that reached
// the environment boundary, and to warn before doing so.
func GitConfigCountForwardMissingPairs(agent string, passthrough, source []string) (names []string, total int) {
	wanted := make(map[string]struct{}, len(passthrough))
	for _, name := range passthrough {
		wanted[strings.TrimSpace(name)] = struct{}{}
	}
	if _, ok := wanted["GIT_CONFIG_COUNT"]; !ok {
		return nil, 0
	}
	count, ok := gitConfigCountInEnv(source)
	if !ok {
		return nil, 0
	}
	return gitPairPresenceForwarded(agent, wanted, source).missing(count, missingPairReportCap)
}

// missingGitConfigCountPairsInEnv is the exec-shim diagnostic counterpart:
// the bounded missing-pair report for a live GIT_CONFIG_COUNT in the
// environment about to reach the agent.
func missingGitConfigCountPairsInEnv(environ []string) (names []string, total int) {
	count, ok := gitConfigCountInEnv(environ)
	if !ok {
		return nil, 0
	}
	return gitPairPresenceInEnv(environ).missing(count, missingPairReportCap)
}

// FormatGitConfigMissingPairs renders the bounded missing-pair report for
// a diagnostic: the capped names plus an "and N more" tail when the exact
// total exceeds them. It names indices only, never values.
func FormatGitConfigMissingPairs(names []string, total int) string {
	if total <= len(names) {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, … and %d more", strings.Join(names, ", "), total-len(names))
}

// NeutralizeIncompleteGitConfigCount returns environ with GIT_CONFIG_COUNT
// pinned defined-empty when its value resolves to a positive count whose
// required indexed pairs are not all usable in environ. It also reports
// whether it neutralized, so callers can surface the operator-facing
// diagnostic the silent-blank would otherwise hide.
//
// Presence is read from the FINAL environment (the one about to reach the
// agent or git): a required pair absent or defined-empty there is unusable
// because the operator did not forward it or forwarded it without a value,
// and git fails either way — "missing config key" for an absent pair,
// "empty config key" for a defined-empty KEY — so the count is pinned
// empty (which strtoul reads as 0, making the whole family inert) rather
// than delivered live. A defined-empty VALUE is a legal empty config value
// and counts as present. A complete forward — count plus every required
// pair present — and a count value git itself rejects are left untouched.
func NeutralizeIncompleteGitConfigCount(environ []string) ([]string, bool) {
	count, ok := gitConfigCountInEnv(environ)
	if !ok {
		return environ, false
	}
	if _, total := gitPairPresenceInEnv(environ).missing(count, 0); total > 0 {
		return forceEnvEntryEmpty(environ, "GIT_CONFIG_COUNT"), true
	}
	return environ, false
}
