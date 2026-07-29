package config

// structTagName returns the field name before any comma-separated tag options.
func structTagName(tag string) string {
	for i := range tag {
		if tag[i] == ',' {
			return tag[:i]
		}
	}
	return tag
}
