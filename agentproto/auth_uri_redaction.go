package agentproto

import (
	"strings"

	"github.com/sachiniyer/agent-factory/internal/redactx"
)

type accessTokenTextSpan struct {
	start int
	end   int
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
	keyView, _ := redactx.PercentDecode(pair[:keyEnd], true)
	if strings.EqualFold(keyView.Text, AccessTokenQueryParam) {
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
// component while retaining a byte map to the original representation — the
// shared nested decoder in redactx. Its stable view covers nested percent
// encoding, while the decoder's reducing stack keeps work bounded by the
// input length.
func redactPercentEncodedAccessTokenText(raw string, plusAsSpace bool) (string, bool) {
	view, _ := redactx.PercentDecode(raw, plusAsSpace)
	spans := accessTokenURIValueSpans(view.Text)
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

// percentDecodedBoundary projects a decoded-view offset to the raw coordinate
// where that offset begins: the source start of the byte at offset, or the
// raw length when the offset lands past the view's end — which is what lets a
// value spanning to the decoded end claim the whole raw tail.
func percentDecodedBoundary(view redactx.View, offset, rawLength int) int {
	if offset >= len(view.Source) {
		return rawLength
	}
	return view.Source[offset].Start
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
