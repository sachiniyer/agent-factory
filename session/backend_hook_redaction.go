package session

import "strings"

// appendSerializedHookJSONReplacement preserves a shared quote boundary between
// malformed adjacent strings. The scanner intentionally reuses every closing
// quote as a possible opener, so two sanitized spans can overlap by that byte.
func appendSerializedHookJSONReplacement(
	redacted *strings.Builder,
	output string,
	written *int,
	start, end int,
	replacement []byte,
) {
	overlap := *written - start
	if overlap <= 0 {
		redacted.WriteString(output[*written:start])
		overlap = 0
	}
	if overlap < len(replacement) {
		redacted.Write(replacement[overlap:])
	}
	if end > *written {
		*written = end
	}
}
