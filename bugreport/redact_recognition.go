package bugreport

// redactionTextKind is provenance, not a guess made from interesting-looking
// bytes. It is the recognition half of the redaction invariant: span union can
// cover every candidate only after the grammar that owns the surrounding text
// has said where each candidate ends.
type redactionTextKind uint8

const (
	redactionTextUnknown redactionTextKind = iota
	redactionTextRendered
	redactionTextLog
	redactionTextDiagnostic
	redactionTextJSONDocument
	redactionTextConfigJSON
	redactionTextConfigTOML
	redactionTextGoQuoted
	redactionTextShell
)

// Recognition model and closed transformation set
//
// Every parser-proven transform produces a logical value plus a byte-for-byte
// map back to its source. The full matcher policy for that value's provenance
// runs on every logical view, so transforms compose instead of being special
// cases in individual path, title, credential, username, account, or tmux-name
// matchers. The admitted transforms are:
//
//   - JSON, TOML, and Go-quoted transport decoding. The owning transport
//     re-encodes a changed value in that same target grammar. TOML comments are
//     separate parser-proven free-text regions; a match never crosses their
//     structural syntax.
//   - POSIX shell quote/escape removal and adjacent literal concatenation, only
//     in command fields proven by the config schema or fixed AF log emitters.
//     Parameter/command/arithmetic/process and pathname expansions split a
//     logical run because their execution-time bytes are absent. Tilde expansion
//     likewise synthesizes no source bytes and therefore no candidate.
//   - Percent decoding of a parser-proven URI path. Query and fragment values
//     remain outside that path view; an independently valid URI inside either is
//     recognized separately, and a query's literal '&' bounds the nested field.
//   - Removal of complete ANSI controls from raw log/diagnostic display text.
//     This makes insertion mid-token zero-width. String-control payloads are a
//     separate nested channel; if one contains a sensitive match, the complete
//     control is replaced rather than preserving an unsafe opaque payload.
//
// Deliberate exclusions follow provenance, not byte shape: '%' is ordinary data
// outside a parsed URI path; shell punctuation is ordinary text outside a proven
// command field; and ANSI-looking bytes inside decoded config or shell values are
// data for their downstream consumer, not terminal presentation. An incomplete
// ESC sequence establishes no ANSI transform. These distinctions avoid turning
// legal Unix filename bytes or user-authored diagnostics into global delimiters.
// A daemon log itself is identified by collectLog; legacy raw hook commands may
// span lines and end only at the exact record prefix configured by log.Initialize.
// A bug-report JSON document is identified at json.Marshal; config field names
// inside it do not establish shell provenance.
//
// Unknown provenance never falls back to a guessed, weaker grammar. The owning
// logical value is replaced with redactedMarker. Parser recovery inside a known
// kind likewise cannot consume the next candidate opener: it resumes at that
// opener, while a declared config document that cannot establish scalar
// boundaries is redacted as one unknown logical value.

func (r *redactor) scrubRecognizedText(s string, kind redactionTextKind) string {
	switch kind {
	case redactionTextRendered:
		return r.scrubGenericText(s)
	case redactionTextLog:
		return r.scrubKnownLogValues(s)
	case redactionTextDiagnostic:
		return r.scrubKnownDiagnosticValues(s)
	default:
		// Config, Go-quoted, and shell values require an owning decoder/parser
		// that can map their safe replacement back to the outer representation.
		// Calling this flat-text entry without that owner is unknown provenance.
		return redactedMarker
	}
}
