package store

import "github.com/sachiniyer/agent-factory/session"

// Row identity is the one rule every selection path asks: "is this row the one
// I had?" Within a projection the answer is the pointer. Across a #765 kill and
// recreate it is the title, because the replacement is a new instance carrying
// the old row's title, and the selection, the tree's fold, and the cursor's tab
// memo all follow it there (#969).
//
// The title half has one exception: the row the create form is naming. Its
// title is a draft, so for a keystroke it can spell an existing session's title.
// Before #7060 every path that matched by title read that as the same row. The
// rail auto-expanded the existing session as if it were the selection, its tab
// rows shifted the list under the cursor, and the cursor committed whichever
// unrelated row slid under it. The snapshot reconcile's re-pin resolved the
// captured title to the existing session too. Both now ask SameRow, so the
// exception lives in one place.

// SetNaming records inst as the row the create form is naming. nil means no
// form is open. Views derive their row lists from SameRow, so a change bumps the
// version like any other mutation they key on.
func (p *Projection) SetNaming(inst *session.Instance) {
	if p.naming == inst {
		return
	}
	p.naming = inst
	p.bump()
}

// Naming returns the row the create form is naming, or nil.
func (p *Projection) Naming() *session.Instance {
	return p.naming
}

// SameRow reports whether a and b are the same rail row for selection. That is
// the same instance, or a #765 same-title replacement of it. Title identity
// never applies to the row being named (#7060), since its title is a draft. nil
// matches nothing.
func (p *Projection) SameRow(a, b *session.Instance) bool {
	if a == nil || b == nil {
		return false
	}
	if a == b {
		return true
	}
	if a == p.naming || b == p.naming {
		return false
	}
	return a.Title == b.Title
}

// Resolve returns the row that now stands for target. That is target itself
// while the projection holds it, otherwise the row that replaced it under the
// same title, otherwise nil. A captured pointer still present always wins, so
// a duplicate title cannot redirect it. Only a pointer the projection dropped
// (a #765 swap) falls back to the title.
func (p *Projection) Resolve(target *session.Instance) *session.Instance {
	if target == nil {
		return nil
	}
	if p.ContainsInstance(target) {
		return target
	}
	for _, inst := range p.instances {
		if p.SameRow(inst, target) {
			return inst
		}
	}
	return nil
}
