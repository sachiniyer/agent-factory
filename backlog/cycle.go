package backlog

import (
	"fmt"
	"strings"
)

// The dependency graph has two nodes per item: its start and its done. Edges
// point from a node to what it needs:
//
//	start(X) -> done(Y)     X waits on Y
//	start(C) -> start(P)    a child inherits what its parent waits on
//	start(C) -> done(B)     B is C's previous sibling under a sequential parent
//	done(X)  -> start(X)    a leaf is done only after it started
//	done(P)  -> done(C)     a parent is done only when its children are
//
// A cycle means some item can never start. Modelling start and done apart is
// what lets siblings wait on each other while an item still cannot wait on its
// own ancestor or descendant.
type node struct {
	id   int
	done bool
}

func (b *Backlog) edges() map[node][]node {
	g := map[node][]node{}
	exists := map[int]bool{}
	for _, it := range b.Items {
		exists[it.ID] = true
	}
	prevChild := map[int]int{}
	for _, it := range b.Items {
		start := node{it.ID, false}
		for _, w := range it.WaitsOn {
			if exists[w] {
				g[start] = append(g[start], node{w, true})
			}
		}
		if it.Parent != 0 && exists[it.Parent] {
			g[start] = append(g[start], node{it.Parent, false})
			p := b.Item(it.Parent)
			if prev, ok := prevChild[it.Parent]; ok && p.Sequential {
				g[start] = append(g[start], node{prev, true})
			}
			prevChild[it.Parent] = it.ID
			pd := node{it.Parent, true}
			g[pd] = append(g[pd], node{it.ID, true})
		}
	}
	for _, it := range b.Items {
		if !b.IsParent(it.ID) {
			d := node{it.ID, true}
			g[d] = append(g[d], node{it.ID, false})
		}
	}
	return g
}

// CheckWaitsOn reports whether id may wait on target without making a cycle.
func (b *Backlog) CheckWaitsOn(id, target int) error {
	if _, err := b.get(id); err != nil {
		return err
	}
	if _, err := b.get(target); err != nil {
		return err
	}
	switch {
	case id == target:
		return fmt.Errorf("%s cannot wait on itself", FormatID(id))
	case b.IsAncestor(target, id):
		return fmt.Errorf("%s cannot wait on %s: %s contains it", FormatID(id), FormatID(target), FormatID(target))
	case b.IsAncestor(id, target):
		return fmt.Errorf("%s cannot wait on %s: %s is inside it", FormatID(id), FormatID(target), FormatID(target))
	}
	g := b.edges()
	from, to := node{target, true}, node{id, false}
	g[to] = append(g[to], from)
	path := findPath(g, from, to)
	if path == nil {
		return nil
	}
	ids := append([]int{id}, collapse(path)...)
	if !b.allWaitEdges(path) {
		return fmt.Errorf("%s cannot wait on %s: that would make a wait cycle (%s)",
			FormatID(id), FormatID(target), formatPath(ids))
	}
	return fmt.Errorf("%s cannot wait on %s: %s already waits on %s (%s)",
		FormatID(id), FormatID(target), FormatID(target), FormatID(id), formatPath(ids))
}

func (b *Backlog) isWaitEdge(from, to node) bool {
	if from.done || !to.done || from.id == to.id {
		return false
	}
	it := b.Item(from.id)
	if it == nil {
		return false
	}
	for _, w := range it.WaitsOn {
		if w == to.id {
			return true
		}
	}
	return false
}

// allWaitEdges reports whether every step of path between two items is a
// waits-on link; steps inside one item (a leaf's done needing its start) are
// allowed.
func (b *Backlog) allWaitEdges(path []node) bool {
	for k := 0; k+1 < len(path); k++ {
		from, to := path[k], path[k+1]
		if from.id == to.id && from.done && !to.done {
			continue
		}
		if !b.isWaitEdge(from, to) {
			return false
		}
	}
	return true
}

func findPath(g map[node][]node, from, to node) []node {
	prev := map[node]node{from: from}
	queue := []node{from}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n == to {
			var path []node
			for ; n != from; n = prev[n] {
				path = append(path, n)
			}
			path = append(path, from)
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			return path
		}
		for _, m := range g[n] {
			if _, seen := prev[m]; !seen {
				prev[m] = n
				queue = append(queue, m)
			}
		}
	}
	return nil
}

// checkCycles returns an error naming one cycle if the backlog has any.
func (b *Backlog) checkCycles() error {
	if c := b.findCycle(); c != nil {
		return fmt.Errorf("that would make a wait cycle (%s); remove a waits-on link first", formatPath(c))
	}
	return nil
}

func (b *Backlog) findCycle() []int {
	found := b.findCycleNodes()
	if found == nil {
		return nil
	}
	ids := collapse(found)
	if len(ids) < 2 || ids[0] != ids[len(ids)-1] {
		ids = append(ids, ids[0])
	}
	return ids
}

// findCycleNodes returns one cycle as nodes, first and last equal, or nil.
func (b *Backlog) findCycleNodes() []node {
	g := b.edges()
	const (
		white = iota
		grey
		black
	)
	color := map[node]int{}
	var stack []node
	var found []node
	var visit func(n node) bool
	visit = func(n node) bool {
		color[n] = grey
		stack = append(stack, n)
		for _, m := range g[n] {
			switch color[m] {
			case grey:
				for i := range stack {
					if stack[i] == m {
						found = append(append([]node(nil), stack[i:]...), m)
						return true
					}
				}
			case white:
				if visit(m) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}
	for _, it := range b.Items {
		for _, n := range []node{{it.ID, false}, {it.ID, true}} {
			if color[n] == white && visit(n) {
				return found
			}
		}
	}
	return nil
}

func collapse(path []node) []int {
	var ids []int
	for _, n := range path {
		if len(ids) == 0 || ids[len(ids)-1] != n.id {
			ids = append(ids, n.id)
		}
	}
	return ids
}

func formatPath(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = FormatID(id)
	}
	return strings.Join(parts, " → ")
}
