package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// The [web] table — browser-UI display preferences (#5500). Its first key is the
// xterm font size, kept PER DEVICE CLASS because the two defaults already differ
// (#5492 halves the phone pitch so agent TUIs stay readable on a narrow pane):
// the web terminal's +/- controls and `af config set` each write the matching
// leaf, and a browser picks up a saved value on its next config read.
//
// Global-only like appearance: a cloned repository must not choose the
// operator's display settings, so the table never joins InRepoConfig or the
// personal project layer.
type WebConfig struct {
	// TerminalFontSizePhone is the xterm font size at the phone breakpoint
	// (max-width: 768px). Defaults to 6.5 — half the desktop pitch (#5492).
	TerminalFontSizePhone float64 `json:"terminal_font_size_phone" toml:"terminal_font_size_phone"`
	// TerminalFontSizeDesktop is the xterm font size at desktop width.
	TerminalFontSizeDesktop float64 `json:"terminal_font_size_desktop" toml:"terminal_font_size_desktop"`
}

const (
	// The compiled-in sizes are today's #5492 behaviour; an absent [web] table —
	// or an unset leaf (0) — is byte-identical to it.
	defaultWebTerminalFontSizePhone   = 6.5
	defaultWebTerminalFontSizeDesktop = 13

	// The allowed range for a typed value — the same bounds the web client
	// clamps its +/- controls to, so a stepped value can never fail this
	// validator. Deliberately the same for both classes: the boundary is a
	// viewport, not a device, and a desktop window dragged narrow keeps its
	// phone-class setting.
	webTerminalFontSizeMin float64 = 4
	webTerminalFontSizeMax float64 = 32
)

// sanitizeWebTerminalFontSize puts a [web] font leaf on the same
// warn-and-default footing as the other range-checked values in validateConfig:
// an absent or zero leaf means unset and falls back to the compiled-in class
// default silently, while a present-but-unusable value (out of range or
// non-finite) warns and falls back. The typed path stays strict — `af config
// set web.terminal_font_size_phone` validates eagerly and rejects up front (the
// deliberate hand-edit-lenient / typed-strict asymmetry #2556 established).
func sanitizeWebTerminalFontSize(key string, value, fallback float64, prettyConfigPath string) float64 {
	if value == 0 {
		return fallback
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < webTerminalFontSizeMin || value > webTerminalFontSizeMax {
		log.WarningLog.Printf("Config issue in %s: %s=%g is outside [%.0f, %.0f]; using default %g",
			prettyConfigPath, key, value, webTerminalFontSizeMin, webTerminalFontSizeMax, fallback)
		return fallback
	}
	return value
}

// validateWebTerminalFontSize is the typed-input validator a leaf write runs
// before it lands — it sees the canonical float spelling canonicalizeScalar
// produced, so a non-number has already been rejected upstream and what remains
// to check here is the range.
func validateWebTerminalFontSize(leaf, value string) error {
	n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return fmt.Errorf("web.%s must be a number, got %q", leaf, value)
	}
	if n < webTerminalFontSizeMin || n > webTerminalFontSizeMax {
		return fmt.Errorf("web.%s must be between %g and %g, got %g", leaf, webTerminalFontSizeMin, webTerminalFontSizeMax, n)
	}
	return nil
}
