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
	//
	// A valid %HH escape can overlap the leading characters of an otherwise
	// literal access_token<...> substring (e.g. %access_token=SECRET, where
	// %ac is a valid hex pair that consumes the 'a'). The decode-based matcher
	// collapses %ac to a single byte, so the decoded view's text no longer
	// contains the access_token= needle, while the raw pair bytes still carry
	// a literal access_token=<value> that net/url re-emits verbatim from
	// RawQuery. Scan the raw pair directly: redactAccessToken split the query
	// on its field separators before this is called, so a pair contains no &
	// or ; and the value cannot extend into a neighbouring field.
	//
	// The two scans are composed rather than treated as mutually exclusive
	// fallback branches: a single pair can carry an earlier overlap-escape
	// occurrence (only the raw scan sees it) plus a later literal
	// access_token= (the decoded scan sees it first), and returning as soon as
	// one pass matched would leave the earlier value in the bytes net/url
	// re-emits. Each pass redacts a suffix following an access_token= key, and
	// the sentinel is not itself an access_token= key, so running the raw scan
	// over the decoded-redacted result only adds redactions or idempotently
	// rewrites the sentinel — it never leaves a credential in place.
	redacted := pair
	found := false
	if r, f := redactPercentEncodedAccessTokenText(pair, true); f {
		redacted = r
		found = true
	}
	if r, f := redactRawAccessTokenValue(redacted, ""); f {
		redacted = r
		found = true
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
// Opaque) — for a case-insensitive access_token= key, tolerating %HH escapes
// anywhere inside the needle, and redacts the value span following each one.
// The value ends at the first byte in terminators, or at the end of raw when
// terminators is empty or none of its bytes occurs after the '='.
//
// This is the raw-bytes mirror of accessTokenURIValueSpans. The decode-based
// matcher in redactPercentEncodedAccessTokenText needs a literal access_token=
// needle in the decoded view, but a valid %HH escape (e.g. %ac) can borrow its
// literal characters from the very word it overlaps: %ac decodes to a single
// byte 0xAC, so the decoded text loses access_token= while the raw bytes still
// carry a literal access_token=<value>. Scanning the raw bytes catches the
// overlap that the decoded view cannot. Callers run the decoded scan first;
// a caller may run this scan over the decoded-redacted result so an earlier
// overlap is not left in place when a later literal field was redacted
// first. Reprocessing a redacted span is idempotent — the sentinel is not
// itself an access_token= key, so the raw scan neither extends nor escapes it.
//
// The matcher additionally tolerates a valid percent escape anywhere INSIDE
// the needle — single (%5F) or NESTED through an extra %25 (e.g. %255F for
// '_', %253D for '=', the same reducing stack redactx.PercentDecode uses),
// so doubly-encodings spell the same needle character and resolve the same
// way — e.g. %access%5Ftoken=SECRET, where the leading %ac collapses the
// 'a' (so the decoded sweep misses) and the in-needle %5F spells the '_' (so
// the raw bytes carry "access%5Ftoken=", not the literal "access_token="),
// and likewise %access%255Ftoken=SECRET, whose in-needle %255F a
// single-level raw matcher would still see as the byte '%' rather than '_'.
// At each needle position the matcher accepts the literal byte
// case-insensitively OR a (possibly nested) percent escape whose final
// decoded byte equals the needle character (case-insensitively). On a
// malformed '%' (fewer than two hex digits following, at any nesting level)
// the candidate aborts, preserving the fail-open-on-reserved behaviour the
// %ac family relies on: the leading '%' is left untouched and the value is
// matched from the literal tail that survives the collapse.
//
// The matched key span is preserved verbatim: on %access_token=SECRET the
// output keeps the leading '%', and on %access%5Ftoken=SECRET the %5F is kept
// in the key — only the value is the redactor's business (#4161 fidelity).
//
// terminators is the component's structural separator set (/ ; ? # for path,
// fragment, and opaque; empty for a query pair, whose separator was already
// split out), so a value never claims a neighbouring field. The function is
// iterative so the same component can carry more than one access_token field.
func redactRawAccessTokenValue(raw, terminators string) (string, bool) {
	needle := AccessTokenQueryParam + "="
	found := false
	for cursor := 0; cursor < len(raw); {
		keyEnd, ok := matchAccessTokenNeedleRaw(raw, cursor, needle)
		if !ok {
			cursor++
			continue
		}
		valueStart := keyEnd
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

// matchAccessTokenNeedleRaw attempts to match needle starting at raw[cursor],
// accepting at each needle position either the literal byte
// case-insensitively or a percent escape (possibly NESTED through an extra
// %25 — e.g. %255F resolves through %25 to '%' then %5F to '_', mirroring
// redactx.PercentDecode's reducing stack) whose final decoded byte equals
// the expected needle character (case-insensitively). It returns the raw
// byte position immediately after the needle's final character (a literal
// byte or the trailing hex digit of the deepest escape), so a caller can
// re-emit the matched key form verbatim while rewriting only the value that
// follows. On a malformed '%' the candidate aborts (returns ok=false): the
// byte is reserved data, not an escape, so a %ac overlap's leading '%'
// stays a non-match while the literal tail that survives the collapse still
// matches from the next cursor position.
func matchAccessTokenNeedleRaw(raw string, cursor int, needle string) (keyEnd int, ok bool) {
	j := 0
	pos := cursor
	for j < len(needle) {
		if pos >= len(raw) {
			return 0, false
		}
		want := needle[j]
		if equalFoldASCII(raw[pos], want) {
			pos++
			j++
			continue
		}
		if next, ok := matchPercentEscapeRaw(raw, pos, want); ok {
			pos = next
			j++
			continue
		}
		return 0, false
	}
	return pos, true
}

// matchPercentEscapeRaw reports whether raw[pos:] contains a percent escape
// that decodes — through arbitrarily nested percent encoding, the same way
// redactx.PercentDecode resolves %255F to '_' via %25→'%' then %5F→'_', and
// %25%35%46 to '_' via %25→'%', %35→'5', %46→'F' then %5F→'_' — to a single
// byte equal to want (case-insensitively in ASCII). It returns the raw byte
// position after the escape, or ok=false when raw[pos] is not a percent
// escape, the escape is malformed, or it decodes to a byte other than want.
//
// The mechanism is a reducing stack modelled on redactx.PercentDecode: push
// raw bytes (the outer application/x-www-form-urlencoded grammar is already
// stripped by the caller, so '+' stays a literal) and collapse the trailing
// %HH triple to its decoded byte whenever it appears. That resolves nested
// escapes uniformly whether the deeper level's hex digits are contiguous raw
// bytes (%255F, %25255F) or each themselves a %HH whose decoded byte is a hex
// digit (%25%35%46), matching redactx.PercentDecode's reducing-stack
// semantics so an in-needle character spelled either way is matched. A
// malformed '%' (no reducible %HH emerging from the following bytes) leaves
// the stack unable to collapse to a single non-'%' byte, so the candidate
// aborts, preserving the fail-open-on-reserved behaviour the #4663 %ac
// family relies on: the leading '%' is left untouched and the value is
// matched from the literal tail that survives the collapse.
func matchPercentEscapeRaw(raw string, pos int, want byte) (newPos int, ok bool) {
	if pos >= len(raw) || raw[pos] != '%' {
		return 0, false
	}
	stack := make([]byte, 0, 4)
	for i := pos; i < len(raw); i++ {
		stack = append(stack, raw[i])
		for len(stack) >= 3 {
			start := len(stack) - 3
			if stack[start] != '%' {
				break
			}
			hi, hiOK := hexValue(stack[start+1])
			lo, loOK := hexValue(stack[start+2])
			if !hiOK || !loOK {
				break
			}
			stack = append(stack[:start], hi<<4|lo)
		}
		// The escape resolves to a single decoded byte once the stack
		// collapses to one element that is not itself a pending '%'. A
		// pending '%' may still combine with following bytes (deeper
		// nesting), so it is not complete yet.
		if len(stack) == 1 && stack[0] != '%' {
			if equalFoldASCII(stack[0], want) {
				return i + 1, true
			}
			return 0, false
		}
	}
	return 0, false
}

// equalFoldASCII reports whether got equals want under ASCII case folding
// (uppercase A-Z folds to lowercase, mirroring indexFoldASCII). Non-ASCII
// bytes fold to themselves, which is fine for the access_token= needle — it
// is pure ASCII.
func equalFoldASCII(got, want byte) bool {
	if got >= 'A' && got <= 'Z' {
		got += 'a' - 'A'
	}
	return got == want
}

// hexValue returns the decimal value of a single ASCII hex digit and ok=true,
// or ok=false for a non-hex byte. Case-insensitive, mirroring redactx.isHex.
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
