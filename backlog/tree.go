package backlog

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Children returns the ids of id's direct children in order. id 0 returns the
// top-level items.
func (b *Backlog) Children(id int) []int {
	var out []int
	for _, it := range b.Items {
		if it.Parent == id {
			out = append(out, it.ID)
		}
	}
	return out
}

// IsParent reports whether id has at least one child.
func (b *Backlog) IsParent(id int) bool {
	for _, it := range b.Items {
		if it.Parent == id {
			return true
		}
	}
	return false
}

// IsAncestor reports whether anc is a strict ancestor of id.
func (b *Backlog) IsAncestor(anc, id int) bool {
	seen := map[int]bool{}
	for it := b.Item(id); it != nil && it.Parent != 0 && !seen[it.ID]; it = b.Item(it.Parent) {
		seen[it.ID] = true
		if it.Parent == anc {
			return true
		}
	}
	return false
}

// subtreeEnd returns the index just past the subtree that starts at index i.
func (b *Backlog) subtreeEnd(i int) int {
	root := b.Items[i].ID
	j := i + 1
	for j < len(b.Items) && b.IsAncestor(root, b.Items[j].ID) {
		j++
	}
	return j
}

func checkCanBeParent(it *Item) error {
	if it.Status == StatusRunning || len(it.Runs) > 0 {
		return fmt.Errorf("%s has runs; it cannot become a parent", FormatID(it.ID))
	}
	return nil
}

// Move places id's whole subtree at a new position. Exactly one field of to
// must be set. Top and Bottom keep the item under its current parent.
type Move struct {
	Before int
	After  int
	Into   int
	Top    bool
	Bottom bool
	// Root makes the item the last top-level item, whatever its parent was.
	// Bottom, by contrast, keeps the item under its current parent.
	Root bool
}

func (m Move) count() int {
	n := 0
	for _, set := range []bool{m.Before != 0, m.After != 0, m.Into != 0, m.Top, m.Bottom, m.Root} {
		if set {
			n++
		}
	}
	return n
}

// Move reorders the backlog. On error b is unchanged.
func (b *Backlog) Move(id int, to Move, now time.Time) error {
	return b.move(id, to, now, true)
}

// move is Move; checkCycles false leaves the dependency check to the caller,
// for a batch that applies many moves and validates the finished graph once.
func (b *Backlog) move(id int, to Move, now time.Time, checkCycles bool) error {
	if to.count() != 1 {
		return errors.New("move needs exactly one of --before, --after, --top, --bottom, --into")
	}
	it, err := b.get(id)
	if err != nil {
		return err
	}
	parent := it.Parent
	var anchor int
	switch {
	case to.Before != 0 || to.After != 0:
		anchor = to.Before + to.After
		a, err := b.get(anchor)
		if err != nil {
			return err
		}
		parent = a.Parent
	case to.Into != 0:
		anchor = to.Into
		parent = to.Into
	case to.Root:
		parent = 0
	}
	if it.Archived && parent != 0 {
		return fmt.Errorf("%s is archived; unarchive it before you move it into a parent", FormatID(id))
	}
	if anchor == id || (anchor != 0 && b.IsAncestor(id, anchor)) {
		return fmt.Errorf("%s cannot move relative to itself or an item inside it", FormatID(id))
	}
	if parent != 0 {
		p, err := b.get(parent)
		if err != nil {
			return err
		}
		if err := checkCanBeParent(p); err != nil {
			return err
		}
		if g := b.ArchivedBy(parent); g != 0 && b.ArchivedBy(id) != g {
			return fmt.Errorf("%s is archived; unarchive it first", FormatID(g))
		}
	}

	c := b.clone()
	start := c.index(id)
	end := c.subtreeEnd(start)
	block := append([]Item(nil), c.Items[start:end]...)
	c.Items = append(c.Items[:start:start], c.Items[end:]...)
	block[0].Parent = parent
	block[0].UpdatedAt = now

	var at int
	switch {
	case to.Before != 0:
		at = c.index(to.Before)
	case to.After != 0:
		at = c.subtreeEnd(c.index(to.After))
	case to.Into != 0, to.Bottom, to.Root:
		at = c.bottomOf(parent)
	case to.Top:
		at = c.topOf(parent)
	}
	c.Items = append(c.Items[:at], append(block, c.Items[at:]...)...)
	if parent != 0 {
		if p := c.Item(parent); p.Status != StatusDraft {
			p.Status = StatusDraft
			p.UpdatedAt = now
		}
	}
	if checkCycles {
		if err := c.checkCycles(); err != nil {
			return err
		}
	}
	*b = *c
	return nil
}

// bottomOf returns the insert index for a new last child of parent.
func (b *Backlog) bottomOf(parent int) int {
	if parent == 0 {
		return len(b.Items)
	}
	return b.subtreeEnd(b.index(parent))
}

// topOf returns the insert index for a new first child of parent.
func (b *Backlog) topOf(parent int) int {
	if parent == 0 {
		return 0
	}
	return b.index(parent) + 1
}

// NewItem is one item for Add. Before and After are optional and must name a
// sibling under Parent.
type NewItem struct {
	Title      string
	Body       string
	Parent     int
	WaitsOn    []int
	Program    string
	Sequential bool
	Before     int
	After      int
}

// Add appends one draft item, at the bottom of its parent by default. On error
// b is unchanged.
func (b *Backlog) Add(in NewItem, now time.Time) (*Item, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, errors.New("an item needs a title")
	}
	if in.Before != 0 && in.After != 0 {
		return nil, errors.New("use --before or --after, not both")
	}
	if in.Parent != 0 {
		p, err := b.get(in.Parent)
		if err != nil {
			return nil, err
		}
		if err := checkCanBeParent(p); err != nil {
			return nil, err
		}
	}
	c := b.clone()
	id := c.allocID()
	c.Items = append(c.Items, Item{
		ID: id, Title: title, Body: in.Body, Status: StatusDraft, Program: in.Program,
		Sequential: in.Sequential, CreatedAt: now, UpdatedAt: now,
	})
	mv := Move{Into: in.Parent, Before: in.Before, After: in.After, Bottom: in.Parent == 0 && in.Before == 0 && in.After == 0}
	if mv.Before != 0 || mv.After != 0 {
		mv.Into = 0
		anchor, err := c.get(mv.Before + mv.After)
		if err != nil {
			return nil, err
		}
		if anchor.Parent != in.Parent {
			return nil, fmt.Errorf("%s is not a child of %s", FormatID(anchor.ID), parentLabel(in.Parent))
		}
	}
	if err := c.Move(id, mv, now); err != nil {
		return nil, err
	}
	if err := c.setWaits(id, in.WaitsOn); err != nil {
		return nil, err
	}
	*b = *c
	return b.Item(id), nil
}

// setWaits replaces id's waits-on list, checking each new edge for a cycle.
func (b *Backlog) setWaits(id int, ids []int) error {
	waits, err := b.resolveWaits(id, ids)
	if err != nil {
		return err
	}
	b.Item(id).WaitsOn = nil
	for _, w := range waits {
		if err := b.CheckWaitsOn(id, w); err != nil {
			return err
		}
		it := b.Item(id)
		it.WaitsOn = append(it.WaitsOn, w)
	}
	return nil
}

func parentLabel(id int) string {
	if id == 0 {
		return "the top level"
	}
	return FormatID(id)
}

func (b *Backlog) allocID() int {
	if b.NextID < 1 {
		b.NextID = 1
	}
	for b.Item(b.NextID) != nil {
		b.NextID++
	}
	id := b.NextID
	b.NextID++
	return id
}

func (b *Backlog) resolveWaits(id int, ids []int) ([]int, error) {
	var out []int
	seen := map[int]bool{}
	for _, w := range ids {
		if w == id {
			return nil, fmt.Errorf("%s cannot wait on itself", FormatID(id))
		}
		if _, err := b.get(w); err != nil {
			return nil, err
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out, nil
}

// Edit changes the fields that are set. On error b is unchanged.
type Edit struct {
	Title      *string
	Body       *string
	Program    *string
	WaitsOn    *[]int
	Sequential *bool
	Paused     *bool
	Archived   *bool
}

func (b *Backlog) Edit(id int, e Edit, now time.Time) (*Item, error) {
	c := b.clone()
	it, err := c.get(id)
	if err != nil {
		return nil, err
	}
	if e.Title != nil {
		t := strings.TrimSpace(*e.Title)
		if t == "" {
			return nil, errors.New("an item needs a title")
		}
		it.Title = t
	}
	if e.Body != nil {
		it.Body = *e.Body
	}
	if e.Program != nil {
		it.Program = *e.Program
	}
	if e.Sequential != nil {
		it.Sequential = *e.Sequential
	}
	if e.WaitsOn != nil {
		if err := c.setWaits(id, *e.WaitsOn); err != nil {
			return nil, err
		}
	}
	if err := c.applyGroup(it, e); err != nil {
		return nil, err
	}
	it.UpdatedAt = now
	if err := c.checkCycles(); err != nil {
		return nil, err
	}
	*b = *c
	return b.Item(id), nil
}
