package apiproto

// Theme is the daemon-resolved user palette shared by API clients. It carries
// the small semantic source contract, not renderer-specific CSS or terminal
// derivatives, so each surface can preserve its own light/dark mode while
// following the same configured colors.
type Theme struct {
	Name                  string `json:"name,omitempty"`
	Foreground            string `json:"foreground"`
	ForegroundStrong      string `json:"foreground_strong"`
	ForegroundMuted       string `json:"foreground_muted"`
	ForegroundDim         string `json:"foreground_dim"`
	Background            string `json:"background"`
	BackgroundSubtle      string `json:"background_subtle"`
	BackgroundPanel       string `json:"background_panel"`
	Accent                string `json:"accent"`
	Success               string `json:"success"`
	Warning               string `json:"warning"`
	Error                 string `json:"error"`
	Info                  string `json:"info"`
	Purple                string `json:"purple"`
	SelectionBackground   string `json:"selection_background"`
	SelectionForeground   string `json:"selection_foreground"`
	PaneBorderDefault     string `json:"pane_border_default"`
	PaneBorderSelected    string `json:"pane_border_selected"`
	PaneBorderInteractive string `json:"pane_border_interactive"`
	PaneBorderPreview     string `json:"pane_border_preview"`
}
