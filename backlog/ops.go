package backlog

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Delete removes a draft that has no runs, no thread and no children. Links
// that wait on it are removed too. It returns id and the ids whose waits-on
// changed. On error b is unchanged.
func (b *Backlog) Delete(id int) ([]int, error) {
	it, err := b.get(id)
	if err != nil {
		return nil, err
	}
	if b.IsParent(id) {
		return nil, fmt.Errorf("%s has children; delete or move them first, or drop it instead", FormatID(id))
	}
	if it.Status != StatusDraft {
		return nil, fmt.Errorf("%s is %s; only a draft can be deleted — drop it instead", FormatID(id), it.Status)
	}
	if len(it.Runs) > 0 || len(it.Thread) > 0 {
		return nil, fmt.Errorf("%s is a draft with runs or a thread; drop it instead", FormatID(id))
	}
	if p := b.Item(it.Parent); p != nil && p.Orchestrator != nil && len(b.Children(p.ID)) == 1 {
		return nil, fmt.Errorf("%s is the last item in %s, which has an orchestrator; clear it first", FormatID(id), FormatID(p.ID))
	}
	i := b.index(id)
	b.Items = append(b.Items[:i], b.Items[i+1:]...)
	changed := []int{id}
	for j := range b.Items {
		other := &b.Items[j]
		if slices.Contains(other.WaitsOn, id) {
			other.WaitsOn = slices.DeleteFunc(other.WaitsOn, func(x int) bool { return x == id })
			changed = append(changed, other.ID)
		}
	}
	return changed, nil
}

// AddNote appends a note to the item's thread. A note is never delivered to a
// session.
func (b *Backlog) AddNote(id int, author, text string, now time.Time) (*Message, error) {
	it, err := b.get(id)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("a note needs text")
	}
	return it.appendMessage(Message{Kind: KindNote, Author: author, Text: text}, now), nil
}

// SettingsChange sets the fields that are not nil.
type SettingsChange struct {
	Runner     *bool
	MaxRunning *int
	OnDone     *string
}

// ApplySettings validates and applies c. On error b is unchanged.
func (b *Backlog) ApplySettings(c SettingsChange) error {
	s := b.Settings
	if c.Runner != nil {
		s.Runner = *c.Runner
	}
	if c.MaxRunning != nil {
		if *c.MaxRunning < 1 || *c.MaxRunning > MaxMaxRunning {
			return fmt.Errorf("max running must be from 1 to %d", MaxMaxRunning)
		}
		s.MaxRunning = *c.MaxRunning
	}
	if c.OnDone != nil {
		if *c.OnDone != OnDoneKeep && *c.OnDone != OnDoneArchive {
			return fmt.Errorf("on done must be %s or %s", OnDoneKeep, OnDoneArchive)
		}
		s.OnDone = *c.OnDone
	}
	b.Settings = s
	return nil
}

// Apply validates and applies the settings that are set. On error b is
// unchanged.
func (c SetBudgetRequest) Apply(b *Budget) error {
	if c.Limit == nil && c.MaxPerRun == nil && c.USDLimit == nil {
		return errors.New("nothing to change — use --limit, --max-per-run or --usd-limit")
	}
	next := *b
	for _, pct := range []struct {
		flag string
		v    *float64
		dst  *float64
	}{{"--limit", c.Limit, &next.Limit}, {"--max-per-run", c.MaxPerRun, &next.MaxPerRun}} {
		if pct.v == nil {
			continue
		}
		if math.IsNaN(*pct.v) || *pct.v < 0 || *pct.v > 100 {
			return fmt.Errorf("%s must be between 0 and 100", pct.flag)
		}
		*pct.dst = *pct.v
	}
	if c.USDLimit != nil {
		if math.IsNaN(*c.USDLimit) || math.IsInf(*c.USDLimit, 0) || *c.USDLimit < 0 {
			return errors.New("--usd-limit must be a finite amount of US dollars, 0 or more")
		}
		next.USDLimit = *c.USDLimit
	}
	*b = next
	return nil
}

// Cost is the metered cost of the run, in US dollars.
func (r RunRef) Cost() float64 {
	total := 0.0
	for _, m := range r.Meter {
		total += m.Cost
	}
	return total
}

// NeededBy returns the ids of the items that wait on id, in tree order.
func (b *Backlog) NeededBy(id int) []int {
	var out []int
	for _, it := range b.Items {
		for _, w := range it.WaitsOn {
			if w == id {
				out = append(out, it.ID)
				break
			}
		}
	}
	return out
}
