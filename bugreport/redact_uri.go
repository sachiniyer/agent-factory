package bugreport

import (
	"net/url"
	"strings"
)

// appendURIPathSpans treats each parser-proven URI path as a nested logical
// value. Percent escapes are decoded for matching and mapped back to their full
// source spelling. Query and fragment text never enter the path view, but an
// independently valid URI inside either receives its own path view.
func (r *redactor) appendURIPathSpans(
	spans []redactionSpan,
	s string,
	produce textSpanProducer,
) []redactionSpan {
	return r.appendSourceMappedURIPathSpans(spans, identitySourceMappedText(s), produce)
}

func (r *redactor) appendSourceMappedURIPathSpans(
	spans []redactionSpan,
	outer sourceMappedText,
	produce textSpanProducer,
) []redactionSpan {
	paths, unknown := sourceMappedURIPaths(outer)
	for _, opaque := range unknown {
		spans = append(spans, redactionSpan{
			start:       outer.source[opaque.start].start,
			end:         outer.source[opaque.end-1].end,
			replacement: redactedMarker,
			priority:    spanQuotedValue,
		})
	}
	for _, path := range paths {
		spans = appendSourceMappedTextSpans(spans, path, produce)
	}
	return spans
}

func sourceMappedURIPaths(outer sourceMappedText) ([]sourceMappedText, []textSourceRange) {
	if len(outer.value) == 0 || len(outer.source) != len(outer.value) {
		return nil, nil
	}
	var paths []sourceMappedText
	var unknown []textSourceRange
	recoveryStart, recoveryEnd, recoveryWork := 0, -1, 0
	queryStart, queryEnd, queryValueEnd := -1, -1, -1
	for start := 0; start < len(outer.value); start++ {
		if !isASCIIAlpha(outer.value[start]) || start > 0 && isURISchemeByte(outer.value[start-1]) {
			continue
		}
		colon := start + 1
		for colon < len(outer.value) && isURISchemeByte(outer.value[colon]) {
			colon++
		}
		if colon >= len(outer.value) || outer.value[colon] != ':' {
			continue
		}
		limit := len(outer.value)
		if start >= queryStart && start < queryEnd {
			if queryValueEnd <= start {
				queryValueEnd = queryEnd
				if ampersand := strings.IndexByte(outer.value[start:queryEnd], '&'); ampersand >= 0 {
					queryValueEnd = start + ampersand
				}
			}
			limit = queryValueEnd
		} else {
			queryValueEnd = -1
		}
		end := recoveryEnd
		if end <= start || end > limit {
			end = colon + 1
			for end < limit && isURIReferenceByte(outer.value[end]) {
				end++
			}
			recoveryStart, recoveryEnd, recoveryWork = start, end, 0
		}
		uriStart := start
		rawURI := outer.value[uriStart:end]
		pathStart, pathEnd, hasPath := rawURIPathRange(rawURI)
		if pathEnd == 0 {
			continue
		}
		// Malformed nested candidates must not turn parser recovery into
		// quadratic work on attacker-controlled log text. Charge both path
		// parsing and the query-tail scan before either can be repeated; after
		// four complete passes over one lexical URI token, fail closed over it.
		candidateWork := pathEnd
		if pathEnd < len(rawURI) && rawURI[pathEnd] == '?' {
			candidateWork += len(rawURI) - pathEnd
		}
		maxWork := 4 * (recoveryEnd - recoveryStart)
		if recoveryWork > maxWork-candidateWork {
			unknown = append(unknown, textSourceRange{start: recoveryStart, end: recoveryEnd})
			start = end - 1
			recoveryEnd = -1
			continue
		}
		recoveryWork += candidateWork
		// Query field boundaries are lexical URI syntax, independent of whether
		// this candidate's path later parses. Recording them first prevents a
		// valid URI found during recovery from absorbing the outer '&' separator.
		if !(uriStart >= queryStart && uriStart < queryEnd) {
			if nestedStart, nestedEnd, ok := rawURIQueryRange(rawURI, pathEnd); ok {
				queryStart, queryEnd = uriStart+nestedStart, uriStart+nestedEnd
				queryValueEnd = -1
			}
		}
		// Validate only through the path boundary. A malformed query or fragment
		// is a separate logical value and cannot revoke path evidence the URI
		// grammar has already established.
		parsed, err := url.Parse(rawURI[:pathEnd])
		if err != nil || parsed.Scheme == "" {
			continue
		}
		// An established URI owns scheme-looking bytes inside its path, but not
		// inside its query or fragment: those may contain an independently
		// self-identifying nested URI. Resume at the path boundary while reusing
		// the lexical end already found, which keeps nested scanning linear. A
		// rejected URI does not advance here, so recovery can find its next opener.
		pathOwnerEnd := uriStart + pathEnd
		start = pathOwnerEnd - 1
		recoveryStart, recoveryWork = pathOwnerEnd, 0
		if !hasPath || parsed.Path == "" {
			continue
		}
		path, ok := decodeSourceMappedURIPath(outer, uriStart+pathStart, uriStart+pathEnd)
		if !ok || path.value != parsed.Path {
			continue
		}
		paths = append(paths, path)
	}
	return paths, unknown
}

// rawURIQueryRange returns the raw query bytes owned by this URI. Literal '&'
// separates outer query fields and therefore bounds a URI nested in one field;
// percent-encoded ampersands remain part of that nested value.
func rawURIQueryRange(raw string, pathEnd int) (int, int, bool) {
	if pathEnd >= len(raw) || raw[pathEnd] != '?' {
		return 0, 0, false
	}
	start, end := pathEnd+1, len(raw)
	if fragment := strings.IndexByte(raw[start:], '#'); fragment >= 0 {
		end = start + fragment
	}
	return start, end, end > start
}

// rawURIPathRange locates the candidate hier-part path bytes that are handed to
// url.Parse for validation. Literal '?' and '#' belong to the URI grammar and
// delimit the query and fragment; their percent-encoded forms remain path bytes.
func rawURIPathRange(raw string) (int, int, bool) {
	colon := strings.IndexByte(raw, ':')
	if colon < 0 {
		return 0, 0, false
	}
	start := colon + 1
	if strings.HasPrefix(raw[start:], "//") {
		start += 2
		for start < len(raw) && raw[start] != '/' && raw[start] != '?' && raw[start] != '#' {
			start++
		}
	}
	end := start
	for end < len(raw) && raw[end] != '?' && raw[end] != '#' {
		end++
	}
	return start, end, end > start
}

func decodeSourceMappedURIPath(outer sourceMappedText, start, end int) (sourceMappedText, bool) {
	if start < 0 || end > len(outer.value) || start >= end {
		return sourceMappedText{}, false
	}
	decoded, err := url.PathUnescape(outer.value[start:end])
	if err != nil {
		return sourceMappedText{}, false
	}
	value := make([]byte, 0, end-start)
	source := make([]textSourceRange, 0, end-start)
	for offset := start; offset < end; {
		if outer.value[offset] == '%' {
			if offset+2 >= end {
				return sourceMappedText{}, false
			}
			hi, hiOK := uriHexValue(outer.value[offset+1])
			lo, loOK := uriHexValue(outer.value[offset+2])
			if !hiOK || !loOK {
				return sourceMappedText{}, false
			}
			value = append(value, hi<<4|lo)
			source = append(source, textSourceRange{
				start: outer.source[offset].start,
				end:   outer.source[offset+2].end,
			})
			offset += 3
			continue
		}
		value = append(value, outer.value[offset])
		source = append(source, outer.source[offset])
		offset++
	}
	if string(value) != decoded {
		return sourceMappedText{}, false
	}
	return sourceMappedText{value: decoded, source: source}, true
}

func uriHexValue(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

// isURIReferenceByte is the RFC 3986 URI-reference alphabet. It only finds the
// candidate token; net/url remains authoritative for whether the candidate and
// its decoded Path are valid.
func isURIReferenceByte(b byte) bool {
	return isASCIIAlpha(b) || b >= '0' && b <= '9' ||
		strings.ContainsRune("-._~:/?#[]@!$&'()*+,;=%", rune(b)) || b >= 0x80
}

func uriWorktreePathBoundary(s string, start, end int) bool {
	return derivedWorktreePathBoundaryWithContext(s, start, end, uriPathStartsAt, uriLogicalPathEndsAt)
}

func uriKnownRootBoundary(s string, start, end int) bool {
	return knownRootTextBoundaryWithContext(s, start, end, uriPathStartsAt, uriLogicalPathEndsAt)
}

func uriPathStartsAt(_ string, start int) bool {
	return start == 0
}

func uriLogicalPathEndsAt(s string, _, end int) bool {
	return end == len(s) || end < len(s) && s[end] == '/'
}
