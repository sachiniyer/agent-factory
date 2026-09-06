package designtokens

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var tokenName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func validate(t Tokens) error {
	for k, c := range t.Colors {
		if !tokenName.MatchString(k) || !hexColor.MatchString(c.Light) || !hexColor.MatchString(c.Dark) || c.Role == "" {
			return fmt.Errorf("invalid color %q", k)
		}
	}
	for k, v := range t.Values {
		if !tokenName.MatchString(k) || strings.ContainsAny(v.CSS, "{};<>\n") || v.CSS == "" || v.Role == "" || v.TUI < 0 {
			return fmt.Errorf("invalid value %q", k)
		}
	}
	requiredMetrics := []string{"type-caption", "type-body", "type-heading", "type-title", "type-display", "space-1", "space-2", "space-3", "space-4", "radius-control", "radius-dialog"}
	if len(t.Values) != len(requiredMetrics) {
		return fmt.Errorf("keep the contract to five type steps, four spaces and two radii")
	}
	for _, k := range requiredMetrics {
		if _, ok := t.Values[k]; !ok {
			return fmt.Errorf("missing metric %q", k)
		}
	}
	expected := map[string]string{"running": "", "ready": "●", "lost": "◌", "dead": "○", "archived": "▧", "limit-reached": "◆"}
	if len(t.States) != len(expected) {
		return fmt.Errorf("expected six liveness states")
	}
	for _, s := range t.States {
		glyph, ok := expected[s.Name]
		if !ok || s.Glyph != glyph || s.Label == "" || s.Color != s.Name {
			return fmt.Errorf("invalid state %q; preserve #1766 glyph contract", s.Name)
		}
		if _, ok = t.Colors[s.Color]; !ok {
			return fmt.Errorf("missing state color %q", s.Color)
		}
		delete(expected, s.Name)
	}
	requiredColors := []string{"surface", "surface-raised", "ink", "ink-muted", "border", "accent", "running", "ready", "lost", "dead", "archived", "limit-reached"}
	if len(t.Colors) != len(requiredColors) {
		return fmt.Errorf("keep the contract to twelve colour roles")
	}
	for _, k := range requiredColors {
		if _, ok := t.Colors[k]; !ok {
			return fmt.Errorf("missing semantic color %q", k)
		}
	}
	if err := validateInkHierarchy(t); err != nil {
		return err
	}
	// These are the only two component backgrounds. Selection uses raised with
	// an accent marker, not a separate fill or a computed custom palette.
	for _, bg := range []string{"surface", "surface-raised"} {
		for _, fg := range append([]string{"ink", "ink-muted", "accent"}, stateColors(t)...) {
			if err := contrastPair(t, fg, bg, 4.5); err != nil {
				return err
			}
		}
		if err := contrastPair(t, "border", bg, 3); err != nil {
			return err
		}
	}
	// The blurred sidebar title reverses muted text into a readable chip.
	if err := contrastPair(t, "surface", "ink-muted", 4.5); err != nil {
		return err
	}
	return contrastPair(t, "surface", "accent", 4.5)
}

func stateColors(t Tokens) []string {
	var out []string
	for _, s := range t.States {
		out = append(out, s.Color)
	}
	return out
}
func contrastPair(t Tokens, fg, bg string, min float64) error {
	a, b := t.Colors[fg], t.Colors[bg]
	for i, pair := range [][2]string{{a.Light, b.Light}, {a.Dark, b.Dark}} {
		ratio := contrast(pair[0], pair[1])
		if ratio < min {
			return fmt.Errorf("%s on %s theme %d contrast %.2f < %.1f", fg, bg, i, ratio, min)
		}
	}
	return nil
}
func contrast(a, b string) float64 {
	x, y := luminance(a), luminance(b)
	if x < y {
		x, y = y, x
	}
	return (x + 0.05) / (y + 0.05)
}
func luminance(s string) float64 {
	n, _ := strconv.ParseUint(s[1:], 16, 32)
	channel := func(v uint64) float64 {
		f := float64(v) / 255
		if f <= 0.04045 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(n>>16) + 0.7152*channel((n>>8)&255) + 0.0722*channel(n&255)
}
