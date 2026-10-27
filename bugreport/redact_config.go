package bugreport

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/internal/redactspan"
	"github.com/sachiniyer/agent-factory/internal/redactx"
)

// scrubConfig establishes the document kind from configSection.Format before
// decoding any scalar. A declared parser failure makes the document one unknown
// logical value and fails closed; it must never fall through to the weaker Go
// string decoder used for %q log fields.
func (r *redactor) scrubConfig(data []byte, format string) string {
	var kind redactionTextKind
	switch format {
	case "json":
		kind = redactionTextConfigJSON
	case "toml":
		kind = redactionTextConfigTOML
	default:
		return r.scrubRecognizedText(string(data), redactionTextUnknown)
	}
	return r.scrubConfigText(string(data), kind)
}

// scrubJSON scrubs an already-encoded JSON document. Its implementation is
// deliberately kept at this seam so the document's target grammar is explicit.
func (r *redactor) scrubJSON(s string) string {
	scalars, ok := redactx.ParseJSONScalars(s, nil)
	if !ok {
		return r.scrubRecognizedText(s, redactionTextUnknown)
	}
	return r.scrubEncodedText(s, redactionTextJSONDocument, scalars, nil)
}

func (r *redactor) scrubConfigText(s string, kind redactionTextKind) string {
	var scalars []redactx.Scalar
	var comments []redactx.Range
	var ok bool
	switch kind {
	case redactionTextConfigJSON:
		scalars, ok = redactx.ParseJSONScalars(s, isConfigShellPath)
	case redactionTextConfigTOML:
		scalars, comments, ok = redactx.ParseTOMLScalars(s, isConfigShellPath)
	}
	if !ok {
		return r.scrubRecognizedText(s, redactionTextUnknown)
	}
	return r.scrubEncodedText(s, kind, scalars, comments)
}

func (r *redactor) scrubEncodedText(
	s string,
	kind redactionTextKind,
	scalars []redactx.Scalar,
	comments []redactx.Range,
) string {
	var spans []redactionSpan
	var crossCredentials []redactionSpan
	if kind == redactionTextConfigTOML {
		crossCredentials = crossingCredentialSpans(s, scalars, comments)
		// TOML comments are parser-owned free text outside scalar tokens. Plan
		// replacements inside each comment payload, never across the document's
		// structural syntax. A credential matcher may still classify material
		// spanning several regions; in that case each overlapping region is
		// redacted independently below so the document remains valid.
		for _, comment := range comments {
			value := s[comment.Start:comment.End]
			inner := toLocalSpans(r.stage().MatchText(value, redactx.ProvConfigScalar))
			if overlapsAnySpan(comment.Start, comment.End, crossCredentials) {
				inner = append(inner, redactionSpan{
					start: 0, end: len(value), replacement: secretMarker, priority: spanCredential,
				})
			}
			spans = appendOffsetSpans(spans, inner, comment.Start)
		}
	}
	for _, scalar := range scalars {
		redacted := secretMarker
		if !overlapsAnySpan(scalar.Start, scalar.End, crossCredentials) {
			prov := redactx.ProvConfigScalar
			if scalar.Shell {
				prov = redactx.ProvConfigShell
			}
			redacted = r.stage().Scrub(scalar.Value, prov)
		}
		if redacted == scalar.Value {
			continue
		}
		replacement, err := encodeStringForGrammar(redacted, kind)
		if err != nil {
			return r.scrubRecognizedText(s, redactionTextUnknown)
		}
		spans = append(spans, redactionSpan{
			start: scalar.Start, end: scalar.End,
			replacement: replacement, priority: spanQuotedValue,
		})
	}
	return applyRedactionSpans(s, spans)
}

func crossingCredentialSpans(
	s string,
	scalars []redactx.Scalar,
	comments []redactx.Range,
) []redactionSpan {
	candidates := appendCredentialSpans(nil, s)
	crossing := make([]redactionSpan, 0, len(candidates))
	for _, candidate := range candidates {
		contained := false
		for _, scalar := range scalars {
			if candidate.start >= scalar.Start && candidate.end <= scalar.End {
				contained = true
				break
			}
		}
		if !contained {
			for _, comment := range comments {
				if candidate.start >= comment.Start && candidate.end <= comment.End {
					contained = true
					break
				}
			}
		}
		if !contained {
			crossing = append(crossing, candidate)
		}
	}
	return crossing
}

func overlapsAnySpan(start, end int, spans []redactionSpan) bool {
	for _, span := range spans {
		if start < span.end && span.start < end {
			return true
		}
	}
	return false
}

func appendOffsetSpans(spans, inner []redactionSpan, offset int) []redactionSpan {
	for _, span := range inner {
		span.start += offset
		span.end += offset
		spans = append(spans, span)
	}
	return spans
}

// encodeStringForGrammar is the transport half of the recognition invariant:
// a decoded replacement must be valid syntax in its TARGET grammar, never just
// in Go's. Each encoded-document owner selects its own encoder here; the
// encoders themselves live with the other grammar machinery in redactx.
func encodeStringForGrammar(value string, kind redactionTextKind) (string, error) {
	switch kind {
	case redactionTextJSONDocument, redactionTextConfigJSON:
		return redactx.EncodeJSONString(value)
	case redactionTextConfigTOML:
		return redactx.EncodeTOMLString(value)
	default:
		return "", fmt.Errorf("unsupported redaction text grammar %d", kind)
	}
}

// isConfigShellPath is the config schema's answer to "is this scalar a shell
// command": the fields whose values run through /bin/sh -c. It is AF-schema
// knowledge, which is why it lives here and is handed to the shared document
// parsers as a predicate rather than being part of them.
func isConfigShellPath(path []string) bool {
	if len(path) == 1 {
		switch path[0] {
		case "on_archive_command", "post_worktree_commands", "sandbox_ssh":
			return true
		}
	}
	if len(path) == 2 {
		return path[0] == "program_overrides" ||
			path[0] == "sandbox" && path[1] == "ssh" ||
			path[0] == "root_agent" && path[1] == "program"
	}
	return len(path) == 3 && path[0] == "root_agents" && path[2] == "program"
}

// toLocalSpans converts the shared stage's spans back to the package-local
// type for the document assembler, which plans replacements in document
// coordinates before applyRedactionSpans commits them.
func toLocalSpans(spans []redactspan.Span) []redactionSpan {
	out := make([]redactionSpan, 0, len(spans))
	for _, span := range spans {
		out = append(out, redactionSpan{
			start: span.Start, end: span.End,
			replacement: span.Replacement, priority: span.Priority,
		})
	}
	return out
}
