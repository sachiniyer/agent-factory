// Package namegen produces short, readable "adjective-noun" session names
// (e.g. "brave-otter") for the autocreate-name placeholder (#2470).
//
// The wordlist lives HERE, in Go, and only here. It is the single source of
// truth the daemon serves to the web (the SuggestSessionName RPC) and the TUI
// calls in-process, so the two surfaces cannot drift and no copy of the list
// ever reaches TypeScript — the #1970 ruling ("serve the list from the daemon;
// do not duplicate it in the client"), which parity/enum_coverage_test.go
// actively enforces by failing on a hardcoded array reappearing in web/src.
package namegen

import "math/rand/v2"

// maxAttempts bounds the collision-avoidance retries. After it, Suggest returns
// its last candidate regardless: the name is only a PLACEHOLDER, and every real
// create still runs through the daemon's authoritative per-repo uniqueness
// (the title_base auto-suffix walk, daemon/manager_create.go), so a rare
// residual collision is resolved there rather than papered over with an endless
// loop here.
const maxAttempts = 20

// adjectives and nouns are curated to be readable and inoffensive: friendly
// adjectives and common colours paired with recognizable animals. Every entry
// is a single lowercase [a-z]+ word, so a joined "adjective-noun" is already a
// clean git-branch/tmux-safe slug that needs no sanitizing and can never equal
// the reserved "root" title. namegen_test.go pins those invariants.
var adjectives = []string{
	"amber", "azure", "bold", "brave", "bright", "calm", "cheerful", "clever",
	"cobalt", "coral", "cozy", "crimson", "curious", "daring", "eager", "emerald",
	"gentle", "golden", "happy", "hazel", "jade", "jolly", "keen", "kind",
	"lively", "lucky", "mellow", "merry", "nimble", "olive", "plucky", "proud",
	"quick", "quiet", "rapid", "ruby", "sapphire", "scarlet", "silver", "snappy",
	"spry", "sunny", "swift", "teal", "tidy", "violet", "vivid", "witty",
}

var nouns = []string{
	"badger", "beaver", "bison", "cougar", "dolphin", "egret", "falcon", "ferret",
	"finch", "fox", "gecko", "gopher", "hare", "heron", "ibis", "iguana",
	"jackal", "jaguar", "kestrel", "koala", "lemur", "lynx", "magpie", "marmot",
	"marten", "narwhal", "newt", "ocelot", "osprey", "otter", "panda", "puffin",
	"quail", "rabbit", "raccoon", "robin", "salmon", "sparrow", "tapir", "urchin",
	"vole", "walrus", "weasel", "wombat", "yak", "zebra",
}

// Generator draws names from the wordlists. The rand source is injectable so a
// test can assert the format and the collision-retry behavior deterministically
// without reaching into a global — a package-global rand written by a test would
// race every other test in the package under -race.
type Generator struct {
	intn func(n int) int
}

// NewGenerator returns a Generator backed by the concurrency-safe global rand
// (math/rand/v2 needs no seeding and is safe for use from multiple goroutines).
func NewGenerator() *Generator {
	return &Generator{intn: rand.IntN}
}

// Random returns one "adjective-noun" name.
func (g *Generator) Random() string {
	return adjectives[g.intn(len(adjectives))] + "-" + nouns[g.intn(len(nouns))]
}

// Suggest returns a name for which taken reports false, regenerating on a
// collision up to maxAttempts times before giving up and returning the last
// candidate anyway (see maxAttempts). taken may be nil, meaning "nothing is
// taken". Callers pass whatever collision rule they enforce downstream — the TUI
// mirrors the daemon's git.TitlesCollide, the daemon checks its live titles — so
// the placeholder it shows is one the same surface would not immediately reject.
func (g *Generator) Suggest(taken func(name string) bool) string {
	name := g.Random()
	for i := 0; i < maxAttempts && taken != nil && taken(name); i++ {
		name = g.Random()
	}
	return name
}

// def is the shared generator behind the package-level helpers.
var def = NewGenerator()

// Suggest returns a collision-avoiding name from the shared generator; see
// (*Generator).Suggest.
func Suggest(taken func(name string) bool) string { return def.Suggest(taken) }
