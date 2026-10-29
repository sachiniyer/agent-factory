package tmux

import "testing"

func TestDetachKeyTmuxName(t *testing.T) {
	for b, want := range map[byte]string{23: "C-w", 1: "C-a", 26: "C-z", 28: `C-\`, 29: "C-]", 30: "C-^", 31: "C-_"} {
		if got, ok := DetachKeyTmuxName(b); !ok || got != want {
			t.Errorf("DetachKeyTmuxName(%d) = %q, %v; want %q", b, got, ok, want)
		}
	}
	for _, b := range []byte{0, 27, 32, 'a'} {
		if _, ok := DetachKeyTmuxName(b); ok {
			t.Errorf("DetachKeyTmuxName(%d) should have no tmux spelling", b)
		}
	}
}
