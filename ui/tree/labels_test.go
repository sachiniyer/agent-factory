package tree

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/session"
)

// TestRenameableTracksLabel checks the renameable rule rather than restating it:
// a kind is renameable IFF changing Name changes what its label renders. That is
// the whole definition of session.TabKindRenameable, and #1813 rejects a rename
// on any other kind precisely because it would write a field no surface reads.
//
// This test lives in tree, not session, because it is the only side of the
// boundary where both halves are reachable: tree imports session, so session
// cannot import tree to compare against textForTab. session's own
// TestTabKindRenameable can only hand-restate the expected booleans — and it
// drifted exactly that way once already, when #1817's TabKindVSCode joined the
// predicate without joining the test.
//
// The kind list is hand-maintained (Go cannot range over a const block), so this
// does not auto-cover a future kind. It does guarantee that any kind listed here
// has its predicate and its label agreeing, which is the failure that actually
// ships: a kind the daemon accepts a rename for, whose label then ignores it.
func TestRenameableTracksLabel(t *testing.T) {
	kinds := []session.TabKind{
		session.TabKindAgent,
		session.TabKindShell,
		session.TabKindProcess,
		session.TabKindWeb,
		session.TabKindVSCode,
	}
	for _, kind := range kinds {
		readsName := textForTab(&session.Tab{Name: "zzz", Kind: kind}) !=
			textForTab(&session.Tab{Name: "qqq", Kind: kind})
		assert.Equal(t, session.TabKindRenameable(kind), readsName,
			"kind %v: TabKindRenameable says %v but its label %s Name",
			kind, session.TabKindRenameable(kind), map[bool]string{true: "reads", false: "ignores"}[readsName])
	}
}

// TestTabGlyphNeverFallsThroughForKnownKinds pins that every kind the session
// package defines has an EXPLICIT glyph arm. The default arm returns
// ProcessTabGlyph ("›", a terminal), which is a safe answer for an unknown kind
// from a newer daemon but a wrong one for a known kind that simply was not added
// here — #1817's TabKindVSCode is an embedded editor with no PTY, and calling it
// a terminal is the one thing the glyph must not do.
func TestTabGlyphNeverFallsThroughForKnownKinds(t *testing.T) {
	assert.Equal(t, AgentTabGlyph, TabGlyph(session.TabKindAgent))
	assert.Equal(t, ShellTabGlyph, TabGlyph(session.TabKindShell))
	assert.Equal(t, ProcessTabGlyph, TabGlyph(session.TabKindProcess))
	assert.Equal(t, WebTabGlyph, TabGlyph(session.TabKindWeb))
	assert.Equal(t, WebTabGlyph, TabGlyph(session.TabKindVSCode),
		"a vscode tab is an embedded no-PTY browser surface, like a web tab — not a terminal")
}
