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

// Recognition model
//
//   - Rendered and free diagnostic text use ordinary filesystem/text boundaries.
//     A URI is a self-identifying nested context: a valid scheme and the parsed
//     URL.Path, not a delimiter list, decide whether the registered path is
//     complete.
//   - A daemon log is identified by the collectLog call site. Valid %q fields
//     are decoded with Go string syntax. Shell syntax is enabled only for
//     command ranges proven by a fixed AF log emitter; shell-looking user/output
//     text is not guessed to be a command. Current emitters quote commands. A
//     legacy raw hook command may span physical lines and ends only when the
//     exact prefix grammar configured by log.Initialize proves a new record.
//     Complete ANSI controls are parsed as zero-width wrappers around paths in
//     log and diagnostic provenance; an arbitrary ESC byte is not a path
//     delimiter.
//   - A config document is identified by configSection.Format. Its string
//     scalars are decoded with that declared JSON or TOML grammar. Only paths
//     inside fields whose config-schema consumer invokes /bin/sh -c use shell
//     word boundaries.
//   - A bug-report JSON document is identified at the json.Marshal call site.
//     Every string token is decoded and re-encoded as JSON; config field names
//     inside that document do not establish shell provenance.
//   - Decoded Go strings and proven shell commands are nested logical values.
//     Their grammar plans spans on the decoded/original value, and the owning
//     transport maps the safe value back into its source representation. POSIX
//     quote and escape removal, adjacent literal segments, parameter/pathname
//     expansion, expansion-driven field splitting, and escaped line
//     continuations therefore affect boundaries only inside a parser-proven
//     shell value. Tilde expansion can materialize a home prefix at execution
//     time, but those bytes are absent from the bug report, so it does not
//     synthesize a candidate or change source boundaries.
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
