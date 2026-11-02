package backlog

import "fmt"

// normalize repairs what a hand edit can break and refuses what it cannot
// repair: ids must be positive and unique, statuses known. Parent links to a
// missing item or in a loop become top-level, bad waits-on links are removed,
// and items are put back in tree order.
func (b *Backlog) normalize() error {
	if b.Items == nil {
		b.Items = []Item{}
	}
	seen := map[int]bool{}
	maxID := 0
	for _, it := range b.Items {
		if it.ID <= 0 {
			return fmt.Errorf("%s has an item with id %d; ids must be positive", BacklogFileName, it.ID)
		}
		if seen[it.ID] {
			return fmt.Errorf("%s has two items with id %s", BacklogFileName, FormatID(it.ID))
		}
		if !it.Status.Valid() {
			return fmt.Errorf("%s: %s has unknown status %q", BacklogFileName, FormatID(it.ID), it.Status)
		}
		seen[it.ID] = true
		maxID = max(maxID, it.ID)
	}
	if b.NextID <= maxID {
		b.NextID = maxID + 1
	}
	for i := range b.Items {
		it := &b.Items[i]
		if !seen[it.Parent] || it.Parent == it.ID {
			it.Parent = 0
		}
		var waits []int
		dup := map[int]bool{}
		for _, w := range it.WaitsOn {
			if w != it.ID && seen[w] && !dup[w] {
				dup[w] = true
				waits = append(waits, w)
			}
		}
		it.WaitsOn = waits
	}
	b.cutParentLoops()
	b.treeOrder()
	for i := range b.Items {
		if b.IsParent(b.Items[i].ID) {
			b.Items[i].Status = StatusDraft
		}
	}
	b.cutWaitCycles()
	b.normalizeOrchestrators()

	s := &b.Settings
	if s.MaxRunning < 1 {
		s.MaxRunning = DefaultMaxRunning
	}
	s.MaxRunning = min(s.MaxRunning, MaxMaxRunning)
	if s.OnDone != OnDoneArchive {
		s.OnDone = OnDoneKeep
	}
	return nil
}

// cutParentLoops makes the item where each parent loop closes top-level, so
// an item that only points into a loop keeps its parent.
func (b *Backlog) cutParentLoops() {
	for i := range b.Items {
		onPath := map[int]bool{}
		for cur := &b.Items[i]; cur != nil && cur.Parent != 0; cur = b.Item(cur.Parent) {
			if onPath[cur.ID] {
				cur.Parent = 0
				break
			}
			onPath[cur.ID] = true
		}
	}
}

// cutWaitCycles drops one waits-on link from each cycle a hand edit left.
func (b *Backlog) cutWaitCycles() {
	for cycle := b.findCycleNodes(); cycle != nil; cycle = b.findCycleNodes() {
		if !b.dropWaitEdge(cycle) {
			return
		}
	}
}

func (b *Backlog) dropWaitEdge(cycle []node) bool {
	for k := 0; k+1 < len(cycle); k++ {
		from, to := cycle[k], cycle[k+1]
		if !b.isWaitEdge(from, to) {
			continue
		}
		it := b.Item(from.id)
		for j, w := range it.WaitsOn {
			if w == to.id {
				it.WaitsOn = append(it.WaitsOn[:j:j], it.WaitsOn[j+1:]...)
				if len(it.WaitsOn) == 0 {
					it.WaitsOn = nil
				}
				return true
			}
		}
	}
	return false
}

func (b *Backlog) treeOrder() {
	kids := map[int][]int{}
	pos := map[int]int{}
	for i, it := range b.Items {
		kids[it.Parent] = append(kids[it.Parent], it.ID)
		pos[it.ID] = i
	}
	out := make([]Item, 0, len(b.Items))
	var walk func(parent int)
	walk = func(parent int) {
		for _, id := range kids[parent] {
			out = append(out, b.Items[pos[id]])
			walk(id)
		}
	}
	walk(0)
	b.Items = out
}
