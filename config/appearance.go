package config

import "fmt"

// NormalizeAppearance mirrors web normalizeThemeChoice. Legacy auto, unknown
// presets and absent values become System; no legacy palette is projected.
func NormalizeAppearance(value string) string {
	if value == "light" || value == "dark" {
		return value
	}
	return "system"
}

func ValidateAppearance(value string) error {
	switch value {
	case "light", "dark", "system":
		return nil
	}
	return fmt.Errorf("appearance must be one of light, dark, system")
}
