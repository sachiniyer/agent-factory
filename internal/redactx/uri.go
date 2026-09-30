package redactx

import (
	"net/url"
	"strings"
)

// uriTransform treats each parser-proven URI inside text as nested logical
// values: the path percent-decodes into a path view, each query pair into a
// pair view, the fragment into a component view. Decoding is STABLE — nested
// escapes resolve through PercentDecode, so a credential does not survive by
// being encoded twice.
//
// Provenance rules:
//   - Query and fragment text never enter the path view, but an independently
//     valid URI inside either is recognized separately by the outer scan, and
//     a query's literal '&' bounds a nested field.
//   - '%' is ordinary data outside a parsed URI — this transform only runs on
//     ranges url.Parse has proven are URI references.
//   - Consumers decide which component views carry secrets: the path views
//     feed the owning family's path matchers; the pair/component views exist
//     for consumers whose secrets travel in query fields (access_token).
type uriTransform struct{}

func (uriTransform) name() string { return "uri" }

// uriAdmitted lists the provenances whose match policy includes the URI scan.
// It mirrors the pre-extraction layout exactly: every view the sensitive and
// generic span producers ran the path scan over — including shell literal
// runs and ANSI payloads — and nothing else.
func (uriTransform) admit(prov Provenance) bool {
	return provIn(prov,
		ProvLogRecord, ProvLogValue, ProvLogShell, ProvLogShellLiteral,
		ProvDiagnostic, ProvGeneric, ProvConfigScalar, ProvConfigShell,
		ProvConfigShellLiteral, ProvANSIPayload)
}

// uriTrigger requires a ':', without which no URI reference can exist. The
// per-byte scheme scan inside is already cheap; this skips it on colon-free
// text.
func (uriTransform) trigger(text string) bool {
	return strings.IndexByte(text, ':') >= 0
}

func (uriTransform) decode(_ *Engine, text string, prov Provenance, _ int) transformResult {
	components, unknown := sourceMappedURIComponents(Identity(text), prov)
	var res transformResult
	res.fail = append(res.fail, unknown...)
	res.views = components
	return res
}

// uriPathProvenance picks the path-view provenance by family: the sensitive
// (known-value) match policy or the generic one, matching which producer the
// parent provenance carried before extraction.
func uriPathProvenance(prov Provenance) Provenance {
	switch prov {
	case ProvGeneric, ProvConfigScalar, ProvConfigShell, ProvConfigShellLiteral:
		return ProvURIPathGeneric
	default:
		return ProvURIPathSensitive
	}
}

// sourceMappedURIPaths is the scan over one view's text for URI candidates.
// Every range it emits indexes into outer.Text.
func sourceMappedURIComponents(outer View, prov Provenance) ([]viewOut, []Range) {
	if len(outer.Source) != len(outer.Text) {
		return nil, nil
	}
	s := outer.Text
	var views []viewOut
	var unknown []Range
	recoveryStart, recoveryEnd, recoveryWork := 0, -1, 0
	queryStart, queryEnd, queryValueEnd := -1, -1, -1
	for start := 0; start < len(s); start++ {
		if !isASCIIAlpha(s[start]) || start > 0 && isURISchemeByte(s[start-1]) {
			continue
		}
		colon := start + 1
		for colon < len(s) && isURISchemeByte(s[colon]) {
			colon++
		}
		if colon >= len(s) || s[colon] != ':' {
			continue
		}
		limit := len(s)
		if start >= queryStart && start < queryEnd {
			if queryValueEnd <= start {
				queryValueEnd = queryEnd
				if ampersand := strings.IndexByte(s[start:queryEnd], '&'); ampersand >= 0 {
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
			for end < limit && isURIReferenceByte(s[end]) {
				end++
			}
			recoveryStart, recoveryEnd, recoveryWork = start, end, 0
		}
		rawURI := s[start:end]
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
			unknown = append(unknown, Range{Start: recoveryStart, End: recoveryEnd})
			start = end - 1
			recoveryEnd = -1
			continue
		}
		recoveryWork += candidateWork
		uriStart := start
		// Query field boundaries are lexical URI syntax, independent of whether
		// this candidate's path later parses. Recording them first prevents a
		// valid URI found during recovery from absorbing the outer '&' separator.
		var query [2]int
		hasQuery := false
		if !(uriStart >= queryStart && uriStart < queryEnd) {
			if nestedStart, nestedEnd, ok := rawURIQueryRange(rawURI, pathEnd); ok {
				queryStart, queryEnd = uriStart+nestedStart, uriStart+nestedEnd
				queryValueEnd = -1
				query = [2]int{queryStart, queryEnd}
				hasQuery = true
			}
		}
		// Validate only through the path boundary. A malformed query or fragment
		// is a separate logical value and cannot revoke path evidence the URI
		// grammar has already established.
		parsed, err := url.Parse(rawURI[:pathEnd])
		if err != nil || parsed.Scheme == "" {
			if hasPath {
				if _, malformed := PercentDecode(s[uriStart+pathStart:uriStart+pathEnd], false); malformed {
					unknown = append(unknown, Range{Start: uriStart + pathStart, End: uriStart + pathEnd})
				}
			}
			// A malformed percent escape in the userinfo makes url.Parse
			// reject the whole URI, landing here, so the userinfo-extraction
			// block below (gated on a successful parse) is never reached.
			// Fail-close the userinfo carrier, gated on its own bytes
			// containing a malformed % — mirroring the path gate above — so a
			// url.Parse failure for any other reason (e.g. a bad authority
			// bracket with clean userinfo) still preserves nested-URI
			// recovery. The authority/userinfo offsets match the extraction
			// block exactly.
			if strings.HasPrefix(rawURI[colon-uriStart+1:], "//") {
				authStart := colon - uriStart + 3
				if at := strings.LastIndexByte(rawURI[authStart:pathStart], '@'); at >= 0 {
					if _, malformed := PercentDecode(s[uriStart+authStart:uriStart+authStart+at], false); malformed {
						unknown = append(unknown, Range{Start: uriStart + authStart, End: uriStart + authStart + at})
					}
				}
			}
			// A malformed percent escape in the host (reg-name) makes url.Parse
			// reject the whole URI by the same route. The host is the one authority
			// component path/query/fragment/userinfo leave unaccounted for, and
			// unlike them it is never extracted as a view the producer can match,
			// so a percent-encoded credential sitting in the host ships verbatim
			// unless its own bytes fail closed. Fail-close the host range — the
			// authority bytes after any userinfo, before the path — gated on its
			// own bytes containing a malformed %, mirroring the path and userinfo
			// gates and reserving the host for non-malformed parse failures so
			// nested-URI recovery is preserved.
			if strings.HasPrefix(rawURI[colon-uriStart+1:], "//") {
				authStart := colon - uriStart + 3
				hostStart := authStart
				if at := strings.LastIndexByte(rawURI[authStart:pathStart], '@'); at >= 0 {
					hostStart = authStart + at + 1
				}
				if hostStart < pathStart {
					if _, malformed := PercentDecode(s[uriStart+hostStart:uriStart+pathStart], false); malformed {
						unknown = append(unknown, Range{Start: uriStart + hostStart, End: uriStart + pathStart})
					}
				}
			}
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
		if hasPath && parsed.Path != "" {
			path, ok := decodeURIPathView(s, uriStart+pathStart, uriStart+pathEnd, parsed.Path)
			if ok {
				views = append(views, viewOut{view: path, prov: uriPathProvenance(prov)})
			}
		} else if parsed.Opaque != "" {
			// mailto:body carries its data outside Path, raw and undecoded.
			// It is the same parser-proven percent carrier a path is — a
			// credential under it decodes or, when the escapes are malformed,
			// the whole body fails closed.
			opaque := s[uriStart+pathStart : uriStart+pathEnd]
			if view, malformed := PercentDecode(opaque, false); malformed {
				unknown = append(unknown, Range{Start: uriStart + pathStart, End: uriStart + pathEnd})
			} else {
				views = append(views, viewOut{view: offsetView(view, uriStart+pathStart), prov: ProvURIComponent})
			}
		}
		// userinfo@ inside an authority is the credential-in-URI carrier:
		// scheme://user:pass@host. It sits before the first path slash, raw
		// and percent-encoded, so it gets its own component view.
		if strings.HasPrefix(rawURI[colon-uriStart+1:], "//") {
			authStart := colon - uriStart + 3 // rawURI offset of the authority
			if at := strings.LastIndexByte(rawURI[authStart:pathStart], '@'); at >= 0 {
				userinfo := s[uriStart+authStart : uriStart+authStart+at]
				if view, malformed := PercentDecode(userinfo, false); !malformed {
					views = append(views, viewOut{view: offsetView(view, uriStart+authStart), prov: ProvURIComponent})
				}
			}
		}
		// Query pairs and the fragment are credential carriers on paths the
		// path view does not reach. Each decodes on its own so a registered
		// secret survives under none of them; consumers whose match policy
		// does not cover component provenance simply see no spans.
		if hasQuery {
			for _, pair := range splitQueryPairs(s, query[0], query[1]) {
				if view, malformed := PercentDecode(s[pair.Start:pair.End], true); !malformed {
					views = append(views, viewOut{view: offsetView(view, pair.Start), prov: ProvURIQueryPair})
				} else {
					unknown = append(unknown, Range{Start: pair.Start, End: pair.End})
				}
			}
		}
		if fragment := strings.IndexByte(rawURI[pathEnd:], '#'); fragment >= 0 {
			fragStart := uriStart + pathEnd + fragment + 1
			if fragStart < uriStart+len(rawURI) {
				if view, malformed := PercentDecode(s[fragStart:uriStart+len(rawURI)], false); !malformed {
					views = append(views, viewOut{view: offsetView(view, fragStart), prov: ProvURIComponent})
				} else {
					unknown = append(unknown, Range{Start: fragStart, End: uriStart + len(rawURI)})
				}
			}
		}
	}
	return views, unknown
}

// offsetView shifts a view decoded from a substring into the parent text's
// coordinates.
func offsetView(v View, offset int) View {
	for i := range v.Source {
		v.Source[i].Start += offset
		v.Source[i].End += offset
	}
	return v
}

// splitQueryPairs yields each &- or ;-separated pair's source range in the
// query region [start,end). Both separators split fields in this grammar; a
// semicolon belonging to a value is percent-encoded and stays inside its pair.
func splitQueryPairs(s string, start, end int) []Range {
	var pairs []Range
	pairStart := start
	for i := start; i < end; i++ {
		if s[i] == '&' || s[i] == ';' {
			if pairStart < i {
				pairs = append(pairs, Range{Start: pairStart, End: i})
			}
			pairStart = i + 1
		}
	}
	if pairStart < end {
		pairs = append(pairs, Range{Start: pairStart, End: end})
	}
	return pairs
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

// decodeURIPathView decodes the URI's path region to its stable form. The
// single-level decode must still agree with url.Parse's Path — that proof is
// what makes the region parser-proven — while the view handed to matchers is
// the fully reduced one, so a credential cannot survive by being encoded a
// second time.
func decodeURIPathView(s string, start, end int, parsedPath string) (View, bool) {
	if start < 0 || end > len(s) || start >= end {
		return View{}, false
	}
	decoded, err := url.PathUnescape(s[start:end])
	if err != nil || decoded != parsedPath {
		return View{}, false
	}
	view, malformed := PercentDecode(s[start:end], false)
	if malformed {
		return View{}, false
	}
	return offsetView(view, start), true
}

// isURIReferenceByte is the RFC 3986 URI-reference alphabet. It only finds the
// candidate token; net/url remains authoritative for whether the candidate and
// its decoded Path are valid.
func isURIReferenceByte(b byte) bool {
	return isASCIIAlpha(b) || b >= '0' && b <= '9' ||
		strings.ContainsRune("-._~:/?#[]@!$&'()*+,;=%", rune(b)) || b >= 0x80
}

func isURISchemeByte(b byte) bool {
	return isASCIIAlpha(b) || b >= '0' && b <= '9' || b == '+' || b == '-' || b == '.'
}

func isASCIIAlpha(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}
