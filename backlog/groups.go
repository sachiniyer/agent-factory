package backlog

import "fmt"

// PausedBy returns the nearest paused ancestor of id, or 0.
func (b *Backlog) PausedBy(id int) int {
	it := b.Item(id)
	if it == nil {
		return 0
	}
	seen := map[int]bool{id: true}
	for p := b.Item(it.Parent); p != nil && !seen[p.ID]; p = b.Item(p.Parent) {
		if p.Paused {
			return p.ID
		}
		seen[p.ID] = true
	}
	return 0
}

// ArchivedBy returns id or its nearest ancestor that is archived, or 0.
func (b *Backlog) ArchivedBy(id int) int {
	seen := map[int]bool{}
	for n := b.Item(id); n != nil && !seen[n.ID]; n = b.Item(n.Parent) {
		if n.Archived {
			return n.ID
		}
		seen[n.ID] = true
	}
	return 0
}

func (b *Backlog) setPaused(it *Item, paused bool) error {
	if !b.IsParent(it.ID) {
		return fmt.Errorf("%s is not a parent; only parents can be paused", FormatID(it.ID))
	}
	if paused == it.Paused {
		return nil
	}
	if !paused && it.Archived {
		return fmt.Errorf("%s is archived; unarchive it first", FormatID(it.ID))
	}
	it.Paused = paused
	return nil
}

// setArchived archives a group, which also pauses it, or unarchives it, which
// leaves it paused.
func (b *Backlog) setArchived(it *Item, archived bool) error {
	if archived == it.Archived {
		return nil
	}
	if !archived {
		it.Archived = false
		return nil
	}
	if it.Parent != 0 || !b.IsParent(it.ID) {
		return fmt.Errorf("%s is not a top-level parent; only groups can be archived", FormatID(it.ID))
	}
	if err := b.checkArchivable(it); err != nil {
		return err
	}
	it.Archived, it.Paused = true, true
	return nil
}

func (b *Backlog) checkArchivable(group *Item) error {
	g := FormatID(group.ID)
	start := b.index(group.ID)
	for _, it := range b.Items[start:b.subtreeEnd(start)] {
		id := FormatID(it.ID)
		switch {
		case it.Status == StatusRunning:
			return fmt.Errorf("%s has a running item (%s); stop it before you archive %s", g, id, g)
		case it.OpenQuestion() != nil:
			return fmt.Errorf("%s has an open question on %s; answer it before you archive %s", g, id, g)
		case it.answerPending():
			return fmt.Errorf("%s has an answer on its way to %s; wait for it before you archive %s", g, id, g)
		}
	}
	return nil
}

func (b *Backlog) applyGroup(it *Item, e Edit) error {
	if e.Archived != nil {
		if err := b.setArchived(it, *e.Archived); err != nil {
			return err
		}
	}
	if e.Paused != nil {
		if err := b.setPaused(it, *e.Paused); err != nil {
			return err
		}
	}
	return nil
}
