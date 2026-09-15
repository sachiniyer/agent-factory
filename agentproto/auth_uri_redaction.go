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
// not as properties inferred by an unstructured text matcher. In this grammar,
// both '&' and a raw ';' separate fields; a semicolon belonging to a value is
// percent-encoded and therefore remains inside its pair. The scan preserves the
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

// redactPercentEncodedAccessTokenText fully decodes a parser-proven URI
// component while retaining a byte map to the original representation. The
// stable view covers nested percent encoding, while the decoder's reducing
// stack keeps work bounded by the input length.
func redactPercentEncodedAccessTokenText(raw string, plusAsSpace bool) (string, bool) {
	view, _ := fullyPercentDecodedView(raw, plusAsSpace)
	spans := accessTokenURIValueSpans(percentDecodedText(view))
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
// representation itself contained a malformed escape. A suffix-reducing stack
// resolves escapes exposed by earlier decoding without rescanning the input, so
// arbitrarily nested encoding still takes linear work. Malformed percent bytes
// in the stable view are ordinary data at the proven outer URI layer.
func fullyPercentDecodedView(raw string, plusAsSpace bool) ([]percentDecodedByte, bool) {
	view := make([]percentDecodedByte, 0, len(raw))
	malformedRaw := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '%' &&
			(i+2 >= len(raw) || !isHex(raw[i+1]) || !isHex(raw[i+2])) {
			malformedRaw = true
		}
		current := percentDecodedByte{
			value:       raw[i],
			sourceStart: i,
			sourceEnd:   i + 1,
		}
		// '+' belongs only to the outer application/x-www-form-urlencoded
		// grammar. A plus produced from %2B is decoded data, not syntax to
		// reinterpret at the next nesting depth.
		if plusAsSpace && current.value == '+' {
			current.value = ' '
		}
		view = append(view, current)

		for len(view) >= 3 {
			start := len(view) - 3
			if view[start].value != '%' {
				break
			}
			high, highOK := hexValue(view[start+1].value)
			low, lowOK := hexValue(view[start+2].value)
			if !highOK || !lowOK {
				break
			}
			decoded := percentDecodedByte{
				value:       high<<4 | low,
				sourceStart: view[start].sourceStart,
				sourceEnd:   view[start+2].sourceEnd,
			}
			view = append(view[:start], decoded)
		}
	}
	return view, malformedRaw
}

func isHex(char byte) bool {
	_, ok := hexValue(char)
	return ok
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

// accessTokenURIValueSpans consumes the rest of the parser-proven URI field.
// Text delimiters such as whitespace and quotes may have been percent-decoded
// from credential bytes, so applying the prose boundary rules here would expose
// a suffix. Query-pair separators were removed before this matcher is called.
func accessTokenURIValueSpans(text string) []accessTokenTextSpan {
	needle := AccessTokenQueryParam + "="
	i := indexFoldASCII(text, needle)
	if i < 0 {
		return nil
	}
	start := i + len(needle)
	return []accessTokenTextSpan{{start: start, end: len(text)}}
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
