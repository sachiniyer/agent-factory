package config

import (
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// inRepoAllowedTableLeaves extends the top-level inRepoAllowedKeys check down
// to the leaves of the [docker] and [ssh] tables. Each set is derived from the
// struct the typed decode targets, so adding a field to DockerConfig/SSHConfig
// admits it here automatically. The lookup folds case against the shape key
// (see inRepoUnknownTableLeaves): the typed decoders match field names
// case-insensitively, so a spelling the decode accepts (for example
// [docker] Image) is known here too, and only spellings the decode drops —
// typos — are reported.
var inRepoAllowedTableLeaves = map[string]map[string]bool{
	"docker": inRepoTableLeavesFor(reflect.TypeOf(DockerConfig{})),
	"ssh":    inRepoTableLeavesFor(reflect.TypeOf(SSHConfig{})),
}

// InRepoUnknownLeaf is one leaf under an in-repo [docker] or [ssh] table that
// no DockerConfig/SSHConfig field matches. The typed decode drops it, so the
// setting it was meant to carry has no effect.
type InRepoUnknownLeaf struct {
	// Path is the config file, in the home-abbreviated form load errors use.
	Path string
	// Table is "docker" or "ssh".
	Table string
	// Key is the leaf exactly as spelled in the file.
	Key string
	// Suggestion is the closest known leaf of Table, or "" when none is near.
	Suggestion string
}

// Message is the one-line warning shown on every surface: CLI stderr, the
// daemon log, and `af doctor`.
func (l InRepoUnknownLeaf) Message() string {
	msg := fmt.Sprintf("in-repo config %s: unknown key %q under [%s] is ignored", l.Path, l.Key, l.Table)
	if l.Suggestion != "" {
		msg += fmt.Sprintf(" — did you mean %s?", l.Suggestion)
	} else {
		msg += fmt.Sprintf(" (known %s keys: %s).", l.Table, strings.Join(sortedKeysFrom(inRepoAllowedTableLeaves[l.Table]), ", "))
	}
	return msg + " A later af release will reject it."
}

// UnknownLeaves reports the [docker]/[ssh] leaves the load dropped because no
// field matches them, sorted by table then key. Empty for a clean file.
func (c *InRepoConfig) UnknownLeaves() []InRepoUnknownLeaf {
	if c == nil {
		return nil
	}
	return append([]InRepoUnknownLeaf(nil), c.unknownLeaves...)
}

// inRepoUnknownTableLeaves walks the shape's [docker]/[ssh] tables (which
// metadataForSource decodes as map[string]any for both TOML and JSON, keeping
// the file's key casing) and returns every leaf not in the struct-tag-derived
// allowlist.
func inRepoUnknownTableLeaves(shape map[string]any, prettyPath string) []InRepoUnknownLeaf {
	var unknown []InRepoUnknownLeaf
	for table, allowed := range inRepoAllowedTableLeaves {
		sub, ok := shape[table].(map[string]any)
		if !ok {
			continue
		}
		for leaf := range sub {
			if allowed[strings.ToLower(leaf)] {
				continue
			}
			unknown = append(unknown, InRepoUnknownLeaf{
				Path:       prettyPath,
				Table:      table,
				Key:        leaf,
				Suggestion: closestLeaf(strings.ToLower(leaf), allowed),
			})
		}
	}
	sort.Slice(unknown, func(i, j int) bool {
		if unknown[i].Table != unknown[j].Table {
			return unknown[i].Table < unknown[j].Table
		}
		return unknown[i].Key < unknown[j].Key
	})
	return unknown
}

// closestLeaf returns the known leaf nearest to key by edit distance, or ""
// when the nearest is too far to be a plausible typo. The bound — at most two
// edits, or a third of the key's length for longer keys — admits a dropped
// underscore (runargs), a transposition (iamge) or a missing letter (hots)
// without suggesting an unrelated key for a genuinely new one.
func closestLeaf(key string, known map[string]bool) string {
	best, bestDist := "", -1
	for _, candidate := range sortedKeysFrom(known) {
		d := editDistance(key, candidate)
		if bestDist < 0 || d < bestDist {
			best, bestDist = candidate, d
		}
	}
	limit := max(2, len(key)/3)
	if bestDist < 0 || bestDist > limit {
		return ""
	}
	return best
}

// editDistance is the Levenshtein distance between a and b, by byte.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

var (
	// inRepoUnknownLeafWarned memoizes each emitted message so a process that
	// resolves the same repo repeatedly (the daemon, a TUI) logs it once.
	inRepoUnknownLeafWarned sync.Map

	interactiveWarningsMu sync.Mutex
	// interactiveWarnings receives config warnings a person running af should
	// see directly. nil (the default) means log-only: the daemon, and the TUI
	// once it owns the terminal, must never write to stderr.
	interactiveWarnings io.Writer
)

// SetInteractiveWarningWriter routes config warnings that need a person's
// attention to w (os.Stderr for an interactive CLI command) in addition to the
// log. Pass nil to make them log-only again.
func SetInteractiveWarningWriter(w io.Writer) {
	interactiveWarningsMu.Lock()
	defer interactiveWarningsMu.Unlock()
	interactiveWarnings = w
}

func warnInRepoUnknownLeaves(leaves []InRepoUnknownLeaf) {
	for _, leaf := range leaves {
		msg := leaf.Message()
		if _, seen := inRepoUnknownLeafWarned.LoadOrStore(msg, struct{}{}); seen {
			continue
		}
		log.WarningLog.Print(msg)
		interactiveWarningsMu.Lock()
		if interactiveWarnings != nil {
			fmt.Fprintln(interactiveWarnings, "warning: "+msg)
		}
		interactiveWarningsMu.Unlock()
	}
}

// resetInRepoUnknownLeafWarnings clears the once-per-message memo so a test
// that asserts the warning is not suppressed by an earlier test's emission.
func resetInRepoUnknownLeafWarnings() {
	inRepoUnknownLeafWarned.Clear()
}

// inRepoTableLeavesFor derives the lowercase leaf names the typed decoder
// accepts under a table-typed in-repo field, from the struct's toml and json
// tags.
func inRepoTableLeavesFor(typ reflect.Type) map[string]bool {
	leaves := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		for _, tag := range []string{field.Tag.Get("toml"), field.Tag.Get("json")} {
			name := structTagName(tag)
			if name == "" || name == "-" {
				continue
			}
			leaves[strings.ToLower(name)] = true
		}
	}
	return leaves
}

// sortedKeysFrom returns the sorted keys of a bool-valued map for
// deterministic messages.
func sortedKeysFrom(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
