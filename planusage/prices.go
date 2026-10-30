package planusage

import (
	"regexp"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// Price is a model's API price in US dollars per million tokens.
type Price = config.ModelPrice

// p builds a row with the standard cache multipliers: writes at 1.25x (5
// minutes) and 2x (1 hour) of input, reads at read.
func p(input, read, output float64) Price {
	return Price{Input: input, CacheWrite5m: input * 1.25, CacheWrite1h: input * 2, CacheRead: read, Output: output}
}

// prices is the built-in API price table. config.toml's
// [backlog.prices."<model>"] adds rows or replaces them.
var prices = map[string]Price{
	"claude-fable-5-1":  p(10, 0.25, 50),
	"claude-mythos-5-1": p(10, 0.25, 50),
	"claude-fable-5":    p(10, 1, 50),
	"claude-mythos-5":   p(10, 1, 50),
	"claude-opus-5-5":   p(4, 0.20, 20),
	"claude-opus-5":     p(5, 0.50, 25),
	"claude-opus-4-8":   p(5, 0.50, 25),
	"claude-opus-4-7":   p(5, 0.50, 25),
	"claude-opus-4-6":   p(5, 0.50, 25),
	"claude-opus-4-5":   p(5, 0.50, 25),
	"claude-opus-4-1":   p(15, 1.50, 75),
	"claude-opus-4":     p(15, 1.50, 75),
	"claude-sonnet-5-5": p(2, 0.20, 10),
	"claude-sonnet-5":   p(2, 0.20, 10),
	"claude-sonnet-4-6": p(3, 0.30, 15),
	"claude-sonnet-4-5": p(3, 0.30, 15),
	"claude-sonnet-4":   p(3, 0.30, 15),
	"claude-haiku-5-5":  p(0.10, 0.01, 0.50),
	"claude-haiku-4-5":  p(1, 0.10, 5),

	"claude-3-7-sonnet": p(3, 0.30, 15),
	"claude-3-5-sonnet": p(3, 0.30, 15),
	"claude-3-5-haiku":  p(0.80, 0.08, 4),
	"claude-3-opus":     p(15, 1.50, 75),
	"claude-3-haiku":    p(0.25, 0.03, 1.25),
}

// tier is a higher price for a message whose prompt (input, cache writes and
// cache reads) is over above tokens.
type tier struct {
	above int64
	price Price
}

// Long prompts cost more on these models: Haiku 5.5 above 100K tokens, and
// Sonnet 4 and 4.5 with the 1M context above 200K (Anthropic's pricing page).
var tiers = map[string]tier{
	"claude-haiku-5-5":  {100_000, p(0.50, 0.05, 2.50)},
	"claude-sonnet-4-5": {200_000, p(6, 0.60, 22.50)},
	"claude-sonnet-4":   {200_000, p(6, 0.60, 22.50)},
}

// fast is the price of fast mode (usage.speed "fast").
var fast = map[string]Price{
	"claude-opus-5-5": p(8, 0.40, 40),
	"claude-opus-5":   p(10, 1, 50),
}

var modelDateSuffix = regexp.MustCompile(`[-@]\d{8}$`)

// normalizeModel maps the ids transcripts carry to price table keys:
// lower case, no "[1m]" context marker, no provider prefix, no date.
func normalizeModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.Index(m, "["); i > 0 {
		m = m[:i]
	}
	if i := strings.LastIndex(m, "claude-"); i > 0 {
		m = m[i:]
	}
	for _, suffix := range []string{"-v1:0", ":0"} {
		m = strings.TrimSuffix(m, suffix)
	}
	return modelDateSuffix.ReplaceAllString(m, "")
}

// PriceFor returns the price of model: an override first, by the id as
// written and then normalized, then the built-in table.
func PriceFor(model string, overrides map[string]Price) (Price, bool) {
	if pr, ok := overridePrice(model, overrides); ok {
		return pr, true
	}
	pr, ok := prices[normalizeModel(model)]
	return pr, ok
}

// priceForUsage is PriceFor with the tier and speed of one message applied.
// An override is used as written.
func priceForUsage(model string, u usage, overrides map[string]Price) (Price, bool) {
	if pr, ok := overridePrice(model, overrides); ok {
		return pr, true
	}
	key := normalizeModel(model)
	pr, ok := prices[key]
	if !ok {
		return Price{}, false
	}
	if f, ok := fast[key]; ok && u.Speed == "fast" {
		pr = f
	}
	if t, ok := tiers[key]; ok && u.prompt() > t.above {
		pr = t.price
	}
	return pr, true
}

func overridePrice(model string, overrides map[string]Price) (Price, bool) {
	if pr, ok := overrides[model]; ok {
		return complete(pr), true
	}
	key := normalizeModel(model)
	for k, pr := range overrides {
		if normalizeModel(k) == key {
			return complete(pr), true
		}
	}
	return Price{}, false
}

// complete fills the cache prices an override left at zero.
func complete(pr Price) Price {
	if pr.CacheWrite5m == 0 {
		pr.CacheWrite5m = pr.Input * 1.25
	}
	if pr.CacheWrite1h == 0 {
		pr.CacheWrite1h = pr.Input * 2
	}
	if pr.CacheRead == 0 {
		pr.CacheRead = pr.Input * 0.1
	}
	return pr
}
