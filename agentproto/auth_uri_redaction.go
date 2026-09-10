package agentproto

import "strings"

type accessTokenTextSpan struct {
	start int
	end   int
}

type percentDecodedByte struct {
	value       byte
	sourceStart int
	sourceEnd   int
}

// redactAccessTokenRawQuery treats separators and escaping as query grammar,
// not as properties inferred by an unstructured text matcher. It preserves the
// original query bytes except for values proven sensitive by that grammar.
func redactAccessTokenRawQuery(raw string) (string, bool) {
	var redacted strings.Builder
	redacted.Grow(len(raw))
	found := false
	for start := 0; ; {
		end := len(raw)
		if separator := strings.IndexAny(raw[start:], "&;"); separator >= 0 {
			end = start + separator
		}
		pair, pairFound := redactAccessTokenQueryPair(raw[start:end])
		redacted.WriteString(pair)
		found = found || pairFound
		if end == len(raw) {
			break
		}
		redacted.WriteByte(raw[end])
		start = end + 1
	}
	return redacted.String(), found
}

func redactAccessTokenQueryPair(pair string) (string, bool) {
	equals := strings.IndexByte(pair, '=')
	keyEnd := equals
	if keyEnd < 0 {
		keyEnd = len(pair)
	}
	keyView, _ := fullyPercentDecodedView(pair[:keyEnd], true)
	if strings.EqualFold(percentDecodedText(keyView), AccessTokenQueryParam) {
		if equals < 0 {
			return pair + "=" + accessTokenRedaction, true
		}
		return pair[:equals+1] + accessTokenRedaction, true
	}

	// Remaining query text may itself carry a URL or access_token field. Match
	// that decoded logical text too, but project only the sensitive span back
	// onto this original pair so neighbouring syntax stays byte-for-byte.
	return redactPercentEncodedAccessTokenText(pair, true)
}

// redactPercentEncodedAccessTokenText repeatedly decodes a parser-proven URI
// component while retaining a byte map to the original representation. The
// repeated pass covers nested percent encoding; every successful pass shortens
// an escape, so reaching a stable view is bounded by the input length.
func redactPercentEncodedAccessTokenText(raw string, plusAsSpace bool) (string, bool) {
	view, _ := fullyPercentDecodedView(raw, plusAsSpace)
	spans := accessTokenTextValueSpans(percentDecodedText(view))
	if len(spans) == 0 {
		return raw, false
	}
	sourceSpans := make([]accessTokenTextSpan, 0, len(spans))
	for _, span := range spans {
		sourceSpans = append(sourceSpans, accessTokenTextSpan{
			start: percentDecodedBoundary(view, span.start, len(raw)),
			end:   percentDecodedBoundary(view, span.end, len(raw)),
		})
	}
	return replaceAccessTokenTextSpans(raw, sourceSpans), true
}

// fullyPercentDecodedView returns the stable decoded text and whether the raw
// representation itself contained a malformed escape. Malformed percent bytes
// in a later view are ordinary data at the proven outer URI layer.
func fullyPercentDecodedView(raw string, plusAsSpace bool) ([]percentDecodedByte, bool) {
	view := make([]percentDecodedByte, len(raw))
	for i := range raw {
		view[i] = percentDecodedByte{value: raw[i], sourceStart: i, sourceEnd: i + 1}
	}
	malformedRaw := false
	for depth := 0; ; depth++ {
		// '+' belongs to the outer application/x-www-form-urlencoded grammar.
		// A plus produced from %2B is decoded data, not syntax to reinterpret.
		next, changed, malformed := decodePercentView(view, plusAsSpace && depth == 0)
		if depth == 0 {
			malformedRaw = malformed
		}
		view = next
		if !changed {
			return view, malformedRaw
		}
	}
}

func decodePercentView(view []percentDecodedByte, plusAsSpace bool) ([]percentDecodedByte, bool, bool) {
	next := make([]percentDecodedByte, 0, len(view))
	changed := false
	malformed := false
	for i := 0; i < len(view); i++ {
		current := view[i]
		if current.value == '%' {
			if i+2 >= len(view) {
				malformed = true
				next = append(next, current)
				continue
			}
			high, highOK := hexValue(view[i+1].value)
			low, lowOK := hexValue(view[i+2].value)
			if !highOK || !lowOK {
				malformed = true
				next = append(next, current)
				continue
			}
			next = append(next, percentDecodedByte{
				value:       high<<4 | low,
				sourceStart: current.sourceStart,
				sourceEnd:   view[i+2].sourceEnd,
			})
			i += 2
			changed = true
			continue
		}
		if plusAsSpace && current.value == '+' {
			current.value = ' '
			changed = true
		}
		next = append(next, current)
	}
	return next, changed, malformed
}

func hexValue(char byte) (byte, bool) {
	switch {
	case char >= '0' && char <= '9':
		return char - '0', true
	case char >= 'a' && char <= 'f':
		return char - 'a' + 10, true
	case char >= 'A' && char <= 'F':
		return char - 'A' + 10, true
	default:
		return 0, false
	}
}

func percentDecodedText(view []percentDecodedByte) string {
	text := make([]byte, len(view))
	for i := range view {
		text[i] = view[i].value
	}
	return string(text)
}

func percentDecodedBoundary(view []percentDecodedByte, offset, rawLength int) int {
	if offset >= len(view) {
		return rawLength
	}
	return view[offset].sourceStart
}

func accessTokenTextValueSpans(text string) []accessTokenTextSpan {
	needle := AccessTokenQueryParam + "="
	var spans []accessTokenTextSpan
	for offset := 0; offset < len(text); {
		i := indexFoldASCII(text[offset:], needle)
		if i < 0 {
			break
		}
		start := offset + i + len(needle)
		end := start
		for end < len(text) && !accessTokenValueEnd(text[end]) {
			end++
		}
		spans = append(spans, accessTokenTextSpan{start: start, end: end})
		offset = end
	}
	return spans
}

func replaceAccessTokenTextSpans(text string, spans []accessTokenTextSpan) string {
	var redacted strings.Builder
	redacted.Grow(len(text))
	cursor := 0
	for _, span := range spans {
		redacted.WriteString(text[cursor:span.start])
		redacted.WriteString(accessTokenRedaction)
		cursor = span.end
	}
	redacted.WriteString(text[cursor:])
	return redacted.String()
}
