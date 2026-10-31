package tmux

import "fmt"

// DetachKeyTmuxName spells the detach byte the way tmux's key table does
// ("C-w"), for a handover where tmux itself is the attached client and af cannot
// watch the bytes: af's own raw-proxy attach intercepts DetachKeyByte, a real
// `tmux attach-session` never would. ok is false for a byte with no ctrl-letter
// spelling.
func DetachKeyTmuxName(b byte) (name string, ok bool) {
	switch {
	case b >= 1 && b <= 26:
		return fmt.Sprintf("C-%c", 'a'+b-1), true
	case b == 28:
		return `C-\`, true
	case b == 29:
		return "C-]", true
	case b == 30:
		return "C-^", true
	case b == 31:
		return "C-_", true
	}
	return "", false
}
