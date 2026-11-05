package tmux

import "testing"

func TestComposerDraftPresent(t *testing.T) {
	t.Run("bare glyph is provably empty", func(t *testing.T) {
		present, provable := ComposerDraftPresent("output\n›\n")
		if present || !provable {
			t.Fatalf("got (%v,%v), want (false,true)", present, provable)
		}
	})
	t.Run("content after the glyph is a draft", func(t *testing.T) {
		present, provable := ComposerDraftPresent("output\n› fix the tests\n")
		if !present || !provable {
			t.Fatalf("got (%v,%v), want (true,true)", present, provable)
		}
	})
	t.Run("submitted echo above a live bare composer is not a draft", func(t *testing.T) {
		present, provable := ComposerDraftPresent("› submitted prompt\nworking...\n›\n")
		if present || !provable {
			t.Fatalf("got (%v,%v), want (false,true)", present, provable)
		}
	})
	t.Run("no composer row is unprovable", func(t *testing.T) {
		present, provable := ComposerDraftPresent("raw output, no composer\nmore output\n")
		if present || provable {
			t.Fatalf("got (%v,%v), want (false,false)", present, provable)
		}
	})
	t.Run("claude glyph", func(t *testing.T) {
		present, provable := ComposerDraftPresent("❯ half a thought\n")
		if !present || !provable {
			t.Fatalf("got (%v,%v), want (true,true)", present, provable)
		}
	})
	t.Run("footer below a blank gap is still provably empty", func(t *testing.T) {
		present, provable := ComposerDraftPresent("❯\n\n  ? for shortcuts\n\n")
		if present || !provable {
			t.Fatalf("got (%v,%v), want (false,true)", present, provable)
		}
	})
	t.Run("draft that opens with blank lines inside a frame is a draft", func(t *testing.T) {
		// #6817 review: composerRegion strands a draft that starts with
		// interior blank rows; the closing border proves the content was
		// interior, not a footer.
		present, provable := ComposerDraftPresent("❯\n\nunfinished text\n──────────\n")
		if !present || !provable {
			t.Fatalf("got (%v,%v), want (true,true)", present, provable)
		}
	})
	t.Run("boxed composer with leading blank draft line", func(t *testing.T) {
		present, provable := ComposerDraftPresent("╭──────╮\n│ ›    │\n│      │\n│ text │\n╰──────╯\n")
		if !present || !provable {
			t.Fatalf("got (%v,%v), want (true,true)", present, provable)
		}
	})
	t.Run("frame closes before any content — empty stands", func(t *testing.T) {
		present, provable := ComposerDraftPresent("❯\n──────────\n? for shortcuts\n")
		if present || !provable {
			t.Fatalf("got (%v,%v), want (false,true)", present, provable)
		}
	})
	// #6817 review: placeholder ghost text is empty input, never a draft; a
	// composer glyph af does not recognize stays unprovable.
	t.Run("old codex placeholder is provably empty", func(t *testing.T) {
		present, provable := ComposerDraftPresent("› Ask Codex to do anything\n")
		if present || !provable {
			t.Fatalf("got (%v,%v), want (false,true)", present, provable)
		}
	})
	t.Run("Ultra placeholder is provably empty", func(t *testing.T) {
		present, provable := ComposerDraftPresent(codexUltraComposer)
		if present || !provable {
			t.Fatalf("got (%v,%v), want (false,true)", present, provable)
		}
	})
	t.Run("claude placeholder is provably empty", func(t *testing.T) {
		present, provable := ComposerDraftPresent(`❯ Try "how do I…"` + "\n")
		if present || !provable {
			t.Fatalf("got (%v,%v), want (false,true)", present, provable)
		}
	})
	// #6817 round-2: placeholder-SHAPED text af cannot match exactly is
	// unprovable, never empty — the safe direction for a draft guard.
	t.Run("rotated claude hint is unprovable", func(t *testing.T) {
		present, provable := ComposerDraftPresent(`❯ Try "npm test" then fix what fails` + "\n")
		if present || provable {
			t.Fatalf("got (%v,%v), want (false,false)", present, provable)
		}
	})
	t.Run("two-line draft starting with Try is unprovable", func(t *testing.T) {
		present, provable := ComposerDraftPresent("› Try \"npm test\"\nthen fix what fails\n")
		if present || provable {
			t.Fatalf("got (%v,%v), want (false,false)", present, provable)
		}
	})
	t.Run("bare unrecognized glyph is unprovable", func(t *testing.T) {
		present, provable := ComposerDraftPresent("»\n")
		if present || provable {
			t.Fatalf("got (%v,%v), want (false,false)", present, provable)
		}
	})
	t.Run("placeholder with a footer below is still provably empty", func(t *testing.T) {
		present, provable := ComposerDraftPresent("» Ask Codex to do anything\n  ← for agents · ? for shortcuts\n")
		if present || !provable {
			t.Fatalf("got (%v,%v), want (false,true)", present, provable)
		}
	})
}
