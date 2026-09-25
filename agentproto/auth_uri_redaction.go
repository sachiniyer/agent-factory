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
	redacted, found := redactPercentEncodedAccessTokenText(pair, true)

	// A valid %HH escape can overlap the leading characters of an otherwise
	// literal access_token<...> substring (e.g. %access_token=SECRET, where
	// %ac is a valid hex pair that consumes the 'a'). The decode-based matcher
	// above collapses %ac to a single byte, so the decoded view's text no
	// longer contains the access_token= needle, while the raw pair bytes
	// still carry a literal access_token=<value> that net/url re-emits
	// verbatim from RawQuery. Scan the decoded-pass output directly so any
	// span the decoded matcher redacted stays untouched. Run unconditionally
	// rather than gating on the decoded scan missing: a co-located pair can
	// carry both such an overlap and a later literal access_token=, where the
	// single-anchor decoded matcher anchors at the trailing literal and a
	// gate would short-circuit this raw scan for the entire pair. The decoded
	// pass's redacted span is the literal REDACTED marker, which contains no
	// access_token= needle, so idempotency (not code-path exclusion) keeps
	// this from reprocessing a span the structured pass already redacted.
	// redactAccessToken split the query on its field separators before this is
	// called, so a pair contains no & or ; and the value cannot extend into a
	// neighbouring field.
	if rawRedacted, rawFound := redactRawAccessTokenValue(redacted, ""); rawFound {
		return rawRedacted, true
	}
	if found {
		return redacted, true
	}
	return pair, false
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

// redactRawAccessTokenValue scans raw — a raw, pre-decode URI component or
// query pair that net/url re-emits verbatim (RawQuery/RawPath/RawFragment,
// Opaque) — for case-insensitive literal access_token= substrings and redacts
// the value span following each one. The value ends at the first byte in
// terminators, or at the end of raw when terminators is empty or none of its
// bytes occurs after the '='.
//
// This is the raw-bytes mirror of accessTokenURIValueSpans. The decode-based
// matcher in redactPercentEncodedAccessTokenText needs a literal access_token=
// needle in the decoded view, but a valid %HH escape (e.g. %ac) can borrow its
// literal characters from the very word it overlaps: %ac decodes to a single
// byte 0xAC, so the decoded text loses access_token= while the raw bytes still
// carry a literal access_token=<value>. Scanning the raw bytes catches the
// overlap that the decoded view cannot. Callers run the decoded scan first and
// feed its output here, so a span the decoded pass redacted is the literal
// REDACTED marker, which contains no access_token= needle — the raw scan can
// only match a genuine surviving access_token= overlap, so idempotency (not a
// code-path gate) keeps this from reprocessing a span the structured pass
// already redacted.
//
// terminators is the component's structural separator set (/ ; ? # for path,
// fragment, and opaque; empty for a query pair, whose separator was already
// split out), so a value never claims a neighbouring field. The function is
// iterative so the same component can carry more than one access_token field.
func redactRawAccessTokenValue(raw, terminators string) (string, bool) {
	needle := AccessTokenQueryParam + "="
	found := false
	for cursor := 0; cursor < len(raw); {
		i := indexFoldASCII(raw[cursor:], needle)
		if i < 0 {
			return raw, found
		}
		i += cursor
		valueStart := i + len(needle)
		valueEnd := len(raw)
		if terminators != "" {
			if pos := strings.IndexAny(raw[valueStart:], terminators); pos >= 0 {
				valueEnd = valueStart + pos
			}
		}
		raw = raw[:valueStart] + accessTokenRedaction + raw[valueEnd:]
		found = true
		cursor = valueStart + len(accessTokenRedaction)
	}
	return raw, found
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
