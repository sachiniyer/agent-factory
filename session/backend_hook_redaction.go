package session

import (
	"encoding/json"
	"strings"
)

func sanitizeSerializedHookJSONString(candidate string, incomplete bool) ([]byte, bool) {
	document := candidate
	if incomplete {
		document += `"`
	}
	var decoded string
	if json.Unmarshal([]byte(document), &decoded) != nil {
		return nil, false
	}
	sanitized := redactHookOutputTokens(decoded)
	if sanitized == decoded {
		return nil, false
	}
	encoded, _ := json.Marshal(sanitized)
	if incomplete {
		encoded = encoded[:len(encoded)-1]
	}
	return encoded, true
}

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
