package redactx

// Provenance records where a text value came from. It is the recognition
// half of the redaction invariant: a transform may decode a value only when
// the grammar that owns the surrounding text has proven the encoding, never
// because the bytes happen to look interesting. Byte shape alone never
// upgrades a value into a stronger grammar.
type Provenance uint8

const (
	// ProvUnknown is text with no owning grammar. Nothing decodes it; an
	// entry point asked to scrub it fails closed over the whole value.
	ProvUnknown Provenance = iota
	// ProvGeneric is free text and bundle scalar values: rendered sections,
	// and the match policy applied to decoded config scalars and comments.
	ProvGeneric
	// ProvLogRecord is a daemon log blob. AF's own emitters make it the
	// richest provenance: %q fields, ANSI in hook output, URIs in error
	// text, and emitter-proven shell commands all decode here.
	ProvLogRecord
	// ProvLogValue is a value decoded out of a log record (e.g. the inside
	// of a %q field): it keeps the log family's decoders.
	ProvLogValue
	// ProvLogShell is a proven /bin/sh -c command inside a log record:
	// emitter prose proved it, so shell quote removal applies.
	ProvLogShell
	// ProvLogShellRaw is the legacy emitter's raw command field — the same
	// proven shell command, but matched only at its path boundaries and its
	// literal runs rather than as one decoded value.
	ProvLogShellRaw
	// ProvLogShellLiteral is one literal run inside a log shell command:
	// quote removal and concatenation already applied; no further decode.
	ProvLogShellLiteral
	// ProvDiagnostic is an af-authored diagnostic quoting a foreign error
	// (tmux, git). Same decoders as a log value.
	ProvDiagnostic
	// ProvConfigScalar is a scalar value or comment inside a config
	// document: matched as generic text, but its bytes are data for a
	// downstream consumer, so no transport decoding applies.
	ProvConfigScalar
	// ProvConfigShell is a config scalar the schema proves is a shell
	// command (on_archive_command, program overrides, ...).
	ProvConfigShell
	// ProvConfigShellLiteral is one literal run inside a config shell
	// command.
	ProvConfigShellLiteral
	// ProvANSIPayload is the data payload of a complete ANSI string
	// control. It is only probed: a match anywhere in it redacts the
	// whole control, since the payload's own grammar is opaque.
	ProvANSIPayload
	// ProvURIPathSensitive is a percent-decoded URI path under the
	// sensitive (known-value) match policy.
	ProvURIPathSensitive
	// ProvURIPathGeneric is a percent-decoded URI path under the generic
	// match policy.
	ProvURIPathGeneric
	// ProvURIQueryPair is one percent-decoded query pair of a URI found in
	// text — the carrier an access_token field hides in.
	ProvURIQueryPair
	// ProvURIComponent is a percent-decoded URI component that is neither
	// path nor query pair (fragment, opaque body).
	ProvURIComponent
	// ProvJSONDocument is a marshalled JSON document (entry kind; the
	// document parser owns it, not the transform dispatcher).
	ProvJSONDocument
	// ProvConfigJSON is a config file in JSON format (entry kind).
	ProvConfigJSON
	// ProvConfigTOML is a config file in TOML format (entry kind).
	ProvConfigTOML
	// ProvGoQuoted is a Go-quoted value handed to an entry point with no
	// owning document grammar; fails closed there.
	ProvGoQuoted
	// ProvShell is a shell command handed to an entry point with no owning
	// document grammar; fails closed there.
	ProvShell
)

func (p Provenance) String() string {
	switch p {
	case ProvUnknown:
		return "unknown"
	case ProvGeneric:
		return "generic"
	case ProvLogRecord:
		return "log-record"
	case ProvLogValue:
		return "log-value"
	case ProvLogShell:
		return "log-shell"
	case ProvLogShellRaw:
		return "log-shell-raw"
	case ProvLogShellLiteral:
		return "log-shell-literal"
	case ProvDiagnostic:
		return "diagnostic"
	case ProvConfigScalar:
		return "config-scalar"
	case ProvConfigShell:
		return "config-shell"
	case ProvConfigShellLiteral:
		return "config-shell-literal"
	case ProvANSIPayload:
		return "ansi-payload"
	case ProvURIPathSensitive:
		return "uri-path-sensitive"
	case ProvURIPathGeneric:
		return "uri-path-generic"
	case ProvURIQueryPair:
		return "uri-query-pair"
	case ProvURIComponent:
		return "uri-component"
	case ProvJSONDocument:
		return "json-document"
	case ProvConfigJSON:
		return "config-json"
	case ProvConfigTOML:
		return "config-toml"
	case ProvGoQuoted:
		return "go-quoted"
	case ProvShell:
		return "shell"
	default:
		return "provenance(?)"
	}
}
