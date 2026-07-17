package config

import (
	"reflect"
	"regexp"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

var themeHexColorRE = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// ThemeConfig is the global-only [theme] table. It is TOML-only because TUI
// colors are a user/host preference, like [keys], and legacy config.json is a
// frozen reader.
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
}

// DefaultThemeConfig returns the approved Zenburn-derived default TUI palette
// (#1389), materialized into first-run config.toml so users can edit it.
func DefaultThemeConfig() ThemeConfig {
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
	}
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
		field := cfgValue.Field(i)
		raw := strings.TrimSpace(field.String())
		fallback := defaultValue.Field(i).String()
		key := tomlTagName(cfgType.Field(i).Tag.Get("toml"))
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
	return reflect.TypeOf(ThemeConfig{}).NumField()
}

func tomlTagName(tag string) string {
	for i := range tag {
		if tag[i] == ',' {
			return tag[:i]
		}
	}
	return tag
}
