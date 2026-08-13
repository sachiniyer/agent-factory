package config

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/sachiniyer/agent-factory/log"
)

var themeHexColorRE = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// ThemeConfig is the global-only theme palette used by both the terminal and
// browser renderers. TOML accepts a named preset (theme = "nord") or the
// established custom [theme] table; legacy config.json remains a frozen reader.
type ThemeConfig struct {
	Foreground            string `json:"foreground" toml:"foreground"`
	ForegroundStrong      string `json:"foreground_strong" toml:"foreground_strong"`
	ForegroundMuted       string `json:"foreground_muted" toml:"foreground_muted"`
	ForegroundDim         string `json:"foreground_dim" toml:"foreground_dim"`
	Background            string `json:"background" toml:"background"`
	BackgroundSubtle      string `json:"background_subtle" toml:"background_subtle"`
	BackgroundPanel       string `json:"background_panel" toml:"background_panel"`
	Accent                string `json:"accent" toml:"accent"`
	Success               string `json:"success" toml:"success"`
	Warning               string `json:"warning" toml:"warning"`
	Error                 string `json:"error" toml:"error"`
	Info                  string `json:"info" toml:"info"`
	Purple                string `json:"purple" toml:"purple"`
	SelectionBackground   string `json:"selection_background" toml:"selection_background"`
	SelectionForeground   string `json:"selection_foreground" toml:"selection_foreground"`
	PaneBorderDefault     string `json:"pane_border_default" toml:"pane_border_default"`
	PaneBorderSelected    string `json:"pane_border_selected" toml:"pane_border_selected"`
	PaneBorderInteractive string `json:"pane_border_interactive" toml:"pane_border_interactive"`
	PaneBorderPreview     string `json:"pane_border_preview" toml:"pane_border_preview"`

	// preset remembers scalar TOML input such as theme = "nord" so a later
	// whole-config save preserves the compact product choice instead of expanding
	// it into nineteen implementation slots. Empty means the user supplied a
	// custom [theme] table.
	preset string
}

const DefaultThemePreset = "nord"

// DefaultThemeConfig returns the Nord-derived default palette selected in
// #3220. The six chromatic families remain Nord; the error and purple text
// slots are lifted within Snow Storm just enough to remain readable on Polar
// Night instead of importing colors from a second palette.
func DefaultThemeConfig() ThemeConfig {
	return ThemeConfig{
		Foreground:            "#D8DEE9",
		ForegroundStrong:      "#ECEFF4",
		ForegroundMuted:       "#C3CBD6",
		ForegroundDim:         "#A7B0BE",
		Background:            "#2E3440",
		BackgroundSubtle:      "#3B4252",
		BackgroundPanel:       "#434C5E",
		Accent:                "#88C0D0",
		Success:               "#A3BE8C",
		Warning:               "#EBCB8B",
		Error:                 "#CC8A91",
		Info:                  "#81A1C1",
		Purple:                "#B590AF",
		SelectionBackground:   "#4C566A",
		SelectionForeground:   "#ECEFF4",
		PaneBorderDefault:     "#4C566A",
		PaneBorderSelected:    "#88C0D0",
		PaneBorderInteractive: "#A3BE8C",
		PaneBorderPreview:     "#B48EAD",
		preset:                DefaultThemePreset,
	}
}

func zenburnThemeConfig() ThemeConfig {
	return ThemeConfig{
		Foreground:            "#DCDCCC",
		ForegroundStrong:      "#FFFFEF",
		ForegroundMuted:       "#989890",
		ForegroundDim:         "#656555",
		Background:            "#3F3F3F",
		BackgroundSubtle:      "#494949",
		BackgroundPanel:       "#4F4F4F",
		Accent:                "#8CD0D3",
		Success:               "#7F9F7F",
		Warning:               "#F0DFAF",
		Error:                 "#CC9393",
		Info:                  "#93E0E3",
		Purple:                "#DC8CC3",
		SelectionBackground:   "#4F4F4F",
		SelectionForeground:   "#FFFFEF",
		PaneBorderDefault:     "#989890",
		PaneBorderSelected:    "#8CD0D3",
		PaneBorderInteractive: "#7F9F7F",
		PaneBorderPreview:     "#DC8CC3",
		preset:                "zenburn",
	}
}

// ThemePreset resolves one supported product-level theme choice. The returned
// value is complete, so callers never need a second defaulting path.
func ThemePreset(name string) (ThemeConfig, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case DefaultThemePreset:
		return DefaultThemeConfig(), true
	case "zenburn":
		return zenburnThemeConfig(), true
	default:
		return ThemeConfig{}, false
	}
}

// Preset reports the named choice that produced this palette. Empty identifies
// an explicit [theme] slot table.
func (t ThemeConfig) Preset() string {
	return t.preset
}

// UnmarshalText admits the compact TOML form: theme = "nord". TOML tables are
// still decoded field-by-field by pelletier, so the established [theme] form
// remains backward compatible.
func (t *ThemeConfig) UnmarshalText(text []byte) error {
	name := strings.ToLower(strings.TrimSpace(string(text)))
	preset, ok := ThemePreset(name)
	if !ok {
		names := []string{DefaultThemePreset, "zenburn"}
		sort.Strings(names)
		return fmt.Errorf("unknown theme preset %q (available: %s)", name, strings.Join(names, ", "))
	}
	*t = preset
	return nil
}

func sanitizeThemeColors(config *Config, prettyConfigPath string) {
	if config == nil {
		return
	}
	defaults := DefaultThemeConfig()
	cfgValue := reflect.ValueOf(&config.Theme).Elem()
	defaultValue := reflect.ValueOf(defaults)
	cfgType := cfgValue.Type()
	for i := 0; i < cfgValue.NumField(); i++ {
		if cfgType.Field(i).PkgPath != "" { // unexported preset metadata
			continue
		}
		field := cfgValue.Field(i)
		raw := strings.TrimSpace(field.String())
		fallback := defaultValue.Field(i).String()
		key := structTagName(cfgType.Field(i).Tag.Get("toml"))
		if key == "" {
			key = cfgType.Field(i).Name
		}
		if !themeHexColorRE.MatchString(raw) {
			log.WarningLog.Printf("config %s: theme.%s=%q is not a #RRGGBB color; using default %s", prettyConfigPath, key, field.String(), fallback)
			field.SetString(fallback)
			continue
		}
		field.SetString("#" + strings.ToUpper(raw[1:]))
	}
}

// ThemeSlotCount returns how many color slots the [theme] table has, read from
// ThemeConfig itself. Surfaces that describe the table to a user (the config
// agent's briefing) must ask rather than hardcode a number: a slot added to
// ThemeConfig would otherwise silently turn that copy into a lie, and there is
// nothing else pinning the two together.
func ThemeSlotCount() int {
	t := reflect.TypeOf(ThemeConfig{})
	count := 0
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath == "" && structTagName(field.Tag.Get("toml")) != "" {
			count++
		}
	}
	return count
}

// marshalConfigTOML preserves a named theme as a scalar while retaining the
// ordinary struct encoder for custom [theme] tables. go-toml's scalar
// TextMarshaler hook cannot conditionally fall back to table encoding, so the
// top-level config encoder replaces only its own generated [theme] section.
func marshalConfigTOML(cfg *Config) ([]byte, error) {
	data, err := toml.Marshal(cfg)
	if err != nil || cfg == nil || cfg.Theme.Preset() == "" {
		return data, err
	}

	marker := []byte("[theme]\n")
	start := bytes.Index(data, marker)
	if start < 0 {
		return nil, fmt.Errorf("marshal config: generated theme table is missing")
	}
	end := len(data)
	if next := bytes.Index(data[start+len(marker):], []byte("\n[")); next >= 0 {
		end = start + len(marker) + next + 1
	}

	withoutTheme := make([]byte, 0, len(data))
	withoutTheme = append(withoutTheme, data[:start]...)
	withoutTheme = append(withoutTheme, data[end:]...)
	insert := len(withoutTheme)
	if bytes.HasPrefix(withoutTheme, []byte("[")) {
		insert = 0
	} else if firstTable := bytes.Index(withoutTheme, []byte("\n[")); firstTable >= 0 {
		insert = firstTable + 1
	}
	line := []byte(fmt.Sprintf("theme = '%s'\n\n", cfg.Theme.Preset()))
	var out bytes.Buffer
	out.Write(withoutTheme[:insert])
	out.Write(line)
	out.Write(withoutTheme[insert:])
	return out.Bytes(), nil
}
