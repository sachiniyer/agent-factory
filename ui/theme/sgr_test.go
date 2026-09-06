package theme

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Compare the bytes actually emitted by Lip Gloss, not RGBA or hex values:
// termenv RGBColor.Sequence uses uint8(f*255), whose float truncation can
// lose a channel step. Tighten to exact bytes when the dependency includes
// https://github.com/muesli/termenv/issues/217.
func TestRoleSGRWithinTokenRounding(t *testing.T) {
	raw, err := os.ReadFile("../../design/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	var tokens struct {
		Colors map[string]struct{ Light, Dark string }
	}
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatal(err)
	}
	for _, dark := range []bool{false, true} {
		renderer := lipgloss.NewRenderer(io.Discard)
		renderer.SetColorProfile(termenv.TrueColor)
		renderer.SetHasDarkBackground(dark)
		for name, token := range tokens.Colors {
			t.Run(fmt.Sprintf("%s/dark=%t", name, dark), func(t *testing.T) {
				hex := token.Light
				if dark {
					hex = token.Dark
				}
				rgb, err := strconv.ParseUint(hex[1:], 16, 24)
				if err != nil {
					t.Fatal(err)
				}
				role, ok := Colors()[name]
				if !ok {
					t.Fatalf("missing generated role %s", name)
				}
				for _, background := range []bool{false, true} {
					style, prefix := renderer.NewStyle().Foreground(role), 38
					if background {
						style, prefix = renderer.NewStyle().Background(role), 48
					}
					got := style.Render("x")
					start, end := fmt.Sprintf("\x1b[%d;2;", prefix), "mx\x1b[0m"
					if !strings.HasPrefix(got, start) || !strings.HasSuffix(got, end) {
						t.Fatalf("unexpected SGR framing: %q", got)
					}
					channels := strings.Split(strings.TrimSuffix(strings.TrimPrefix(got, start), end), ";")
					if len(channels) != 3 {
						t.Fatalf("expected three RGB channels: %q", got)
					}
					for i, want := range []int{int(rgb >> 16), int(rgb >> 8 & 255), int(rgb & 255)} {
						emitted, err := strconv.Atoi(channels[i])
						if err != nil || emitted < 0 || emitted > 255 || emitted-want < -1 || emitted-want > 1 {
							t.Errorf("token %s channel %d: emitted %q, want %d ± 1", hex, i, channels[i], want)
						}
					}
				}
			})
		}
	}
}
