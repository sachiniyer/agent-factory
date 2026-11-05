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
}
