package backlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Ref names an item in a batch: a key local to the batch, or an existing id.
type Ref struct {
	Key string
	ID  int
}

func (r Ref) IsZero() bool { return r.Key == "" && r.ID == 0 }

// MarshalJSON writes a zero reference as null, which UnmarshalJSON accepts and
// AddBatch ignores, so a batch the daemon accepts survives being re-serialized
// (a remote af backlog add --file sends it over HTTP). Writing 0 would be
// refused as "item id 0 must be positive".
func (r Ref) MarshalJSON() ([]byte, error) {
	if r.IsZero() {
		return []byte("null"), nil
	}
	if r.Key != "" {
		return json.Marshal(r.Key)
	}
	return json.Marshal(r.ID)
}

func (r *Ref) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*r = Ref{}
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*r = Ref{Key: s}
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("an item reference must be a key string or an item id number: %w", err)
	}
	if n <= 0 {
		return fmt.Errorf("item id %d must be positive", n)
	}
	*r = Ref{ID: n}
	return nil
}

func (r Ref) String() string {
	if r.Key != "" {
		return fmt.Sprintf("key %q", r.Key)
	}
	return FormatID(r.ID)
}

// Batch is the input of `af backlog add --file`.
type Batch struct {
	Proposal string      `json:"proposal,omitempty"`
	Items    []ItemInput `json:"items"`
}

type ItemInput struct {
	Key        string `json:"key,omitempty"`
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	Parent     Ref    `json:"parent,omitzero"`
	WaitsOn    []Ref  `json:"waits_on,omitempty"`
	Program    string `json:"program,omitempty"`
	Sequential bool   `json:"sequential,omitempty"`
}

// AddBatch adds or updates many items at once. With a proposal set, an input
// whose (proposal, key) matches an existing item updates that item's title,
// body, program, sequential flag, parent and waits-on; every other input adds
// a new draft. It returns each key's id. On error b is unchanged.
func (b *Backlog) AddBatch(in Batch, now time.Time) (map[string]int, error) {
	if len(in.Items) == 0 {
		return nil, errors.New("the batch has no items")
	}
	c := b.clone()
	ids := map[string]int{}
	ordered := make([]int, len(in.Items))
	for i, x := range in.Items {
		if strings.TrimSpace(x.Title) == "" {
			return nil, fmt.Errorf("item %d in the batch needs a title", i+1)
		}
		if in.Proposal != "" && x.Key == "" {
			return nil, fmt.Errorf("item %d in the batch needs a key, because the batch names proposal %q", i+1, in.Proposal)
		}
		if x.Key != "" {
			if _, dup := ids[x.Key]; dup {
				return nil, fmt.Errorf("key %q is used twice in the batch", x.Key)
			}
		}
		id := 0
		if in.Proposal != "" {
			id = c.bySource(in.Proposal, x.Key)
		}
		if id == 0 {
			id = c.allocID()
			it := Item{ID: id, Status: StatusDraft, CreatedAt: now}
			if in.Proposal != "" {
				it.Source = &Source{Proposal: in.Proposal, Key: x.Key}
			}
			c.Items = append(c.Items, it)
			ordered[i] = -id
		} else {
			ordered[i] = id
		}
		if x.Key != "" {
			ids[x.Key] = id
		}
	}
	resolve := func(r Ref) (int, error) {
		if r.Key != "" {
			id, ok := ids[r.Key]
			if !ok {
				return 0, fmt.Errorf("key %q is not in the batch", r.Key)
			}
			return id, nil
		}
		if _, err := c.get(r.ID); err != nil {
			return 0, err
		}
		return r.ID, nil
	}

	parents := make([]int, len(in.Items))
	for i, x := range in.Items {
		id := abs(ordered[i])
		it := c.Item(id)
		it.Title = strings.TrimSpace(x.Title)
		it.Body = x.Body
		it.Program = x.Program
		it.Sequential = x.Sequential
		it.UpdatedAt = now
		it.WaitsOn = nil
		if x.Parent.IsZero() {
			continue
		}
		p, err := resolve(x.Parent)
		if err != nil {
			return nil, err
		}
		if p == id {
			return nil, fmt.Errorf("%s cannot be its own parent", x.Parent)
		}
		parents[i] = p
	}

	if err := c.placeBatch(ordered, parents, now); err != nil {
		return nil, err
	}

	for i, x := range in.Items {
		id := abs(ordered[i])
		waits := make([]int, 0, len(x.WaitsOn))
		for _, r := range x.WaitsOn {
			if r.IsZero() {
				continue
			}
			w, err := resolve(r)
			if err != nil {
				return nil, err
			}
			waits = append(waits, w)
		}
		if err := c.setWaits(id, waits); err != nil {
			return nil, err
		}
	}
	if err := c.checkCycles(); err != nil {
		return nil, err
	}
	*b = *c
	return ids, nil
}

// placeBatch moves every batch item under its requested parent. New items
// (negative in ordered) go to the bottom of their parent; existing items move
// only when their parent changed.
//
// It works from the FINAL tree: it first checks that the parent links that
// result (the batch's for its items, the current ones for everything else) have
// no loop, then applies moves in passes, each pass taking every move that is
// legal in the tree as it stands (an item cannot move into its own subtree, and
// an intermediate parent the batch leaves alone may still sit below an item the
// batch is about to move). Because the final tree is acyclic, some pending move
// is always legal, so the passes finish whatever the input order. It does not
// check dependency cycles; the caller checks the finished graph.
func (b *Backlog) placeBatch(ordered, parents []int, now time.Time) error {
	want := map[int]int{}
	for i, o := range ordered {
		want[abs(o)] = parents[i]
	}
	for _, it := range b.Items {
		for cur, steps := it.ID, 0; cur != 0; steps++ {
			if steps > len(b.Items) {
				return errors.New("the batch's parent links form a loop")
			}
			if p, inBatch := want[cur]; inBatch {
				cur = p
			} else {
				cur = b.Item(cur).Parent
			}
		}
	}
	pending := map[int]int{}
	for i, o := range ordered {
		pending[i] = o
	}
	for len(pending) > 0 {
		progress := false
		for i, o := range ordered {
			if _, ok := pending[i]; !ok {
				continue
			}
			id, p := abs(o), parents[i]
			if o >= 0 && b.Item(id).Parent == p {
				delete(pending, i)
				progress = true
				continue
			}
			if p != 0 && (p == id || b.IsAncestor(id, p)) {
				continue // legal once the batch relocates id's descendant p
			}
			// Root, not Bottom: an existing item whose parent was removed must
			// leave it (Bottom keeps the current parent). The dependency check
			// waits for the finished batch (AddBatch), because a valid final
			// tree can pass through a cyclic intermediate one.
			if err := b.move(id, Move{Into: p, Root: p == 0}, now, false); err != nil {
				return err
			}
			delete(pending, i)
			progress = true
		}
		if !progress {
			return errors.New("the batch's parent links form a loop")
		}
	}
	return nil
}

func (b *Backlog) bySource(proposal, key string) int {
	for _, it := range b.Items {
		if it.Source != nil && it.Source.Proposal == proposal && it.Source.Key == key {
			return it.ID
		}
	}
	return 0
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
