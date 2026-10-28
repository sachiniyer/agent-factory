package designtokens

import (
	"fmt"
	"strings"
)

// This product hierarchy floor supplements, rather than replaces, text/background
// readability. It is fixed policy, not another customizable design token.
const inkHierarchyMinimum = 1.5

func (c Color) value(mode string) string {
	if mode == "light" {
		return c.Light
	}
	return c.Dark
}

func validateInkHierarchy(t Tokens) error {
	stateRole := make(map[string]bool, len(t.States))
	for _, state := range t.States {
		stateRole[state.Color] = true
	}
	for role, c := range t.Colors {
		for mode, reason := range c.SharesMuted {
			if !stateRole[role] || (mode != "light" && mode != "dark") || strings.TrimSpace(reason) == "" {
				return fmt.Errorf("%s sharesMuted.%s requires a liveness role, valid theme and rationale", role, mode)
			}
		}
	}
	for _, mode := range []string{"light", "dark"} {
		ink, muted := t.Colors["ink"].value(mode), t.Colors["ink-muted"].value(mode)
		if ratio := contrast(ink, muted); ratio < inkHierarchyMinimum {
			return fmt.Errorf("ink-muted/ink hierarchy in %s is %.2f:1; need at least %.1f:1", mode, ratio, inkHierarchyMinimum)
		}
		for _, background := range []string{"surface", "surface-raised"} {
			bg := t.Colors[background].value(mode)
			if contrast(muted, bg) >= contrast(ink, bg) {
				return fmt.Errorf("ink-muted must be secondary to ink on %s in %s", background, mode)
			}
		}
		for _, state := range t.States {
			color := t.Colors[state.Color]
			matches := strings.EqualFold(color.value(mode), muted)
			_, marked := color.SharesMuted[mode]
			if matches && !marked {
				return fmt.Errorf("%s equals ink-muted in %s; record an intentional sharesMuted rationale", state.Color, mode)
			}
			if !matches && marked {
				return fmt.Errorf("%s sharesMuted.%s is stale: values no longer match", state.Color, mode)
			}
		}
	}
	return nil
}
