package config

import "reflect"

func validateLoadedConfigSchemaVersion(version int, prettyConfigPath string) error {
	if version > GlobalConfigSchemaVersion {
		return &UnsupportedSchemaVersionError{
			StoreName:        "config",
			Path:             prettyConfigPath,
			FileVersion:      version,
			SupportedVersion: GlobalConfigSchemaVersion,
		}
	}
	return nil
}

// knownJSONConfigKeys returns the set of top-level JSON keys the frozen
// Config schema recognizes, derived from the struct's json tags so it cannot
// drift from the fields. Fields tagged json:"-" (e.g. the TOML-only keymap)
// are deliberately excluded — they are not valid config.json keys.
func knownJSONConfigKeys() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		name := jsonTagName(t.Field(i).Tag.Get("json"))
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

func jsonTagName(tag string) string {
	for i := range tag {
		if tag[i] == ',' {
			return tag[:i]
		}
	}
	return tag
}
