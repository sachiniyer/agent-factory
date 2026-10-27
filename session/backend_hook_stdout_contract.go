package session

// The hook scripts' stdout contract: launch_cmd and provision_cmd each echo ONE
// JSON record on stdout and nothing else (docs/remote-hooks.md), so reading
// either is a parse, not a classification — one value, of the documented shape,
// surrounded by whitespace only. This file holds that shared discipline: the
// endpoint record's schema and parse (hookEndpointJSON, decodeHookEndpointJSON,
// parseHookEndpoint), the violation type both parsers return
// (hookStdoutViolation — parseHookProvisionRecord in
// backend_hook_provision.go is the sibling), and the redaction-bounded error
// rendering that reports what a script printed without leaking its tokens
// (hookStdoutExcerpt, hookOutputSuffix). The provisioner state machine that
// runs the scripts — hookProvisioner, launch, reap, quiesceLaunchGroup — stays
// in backend_hook_backend.go; this file is the stream contract both hook
// scripts answer to.
// Split out of backend_hook_backend.go as a file-length decomposition (#1145)
// — same package, so nothing here is a new boundary.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// hookEndpointJSON is the object launch_cmd echoes: the authed `af agent-server`
// endpoint the daemon dials — the same {url,token} docker/ssh read from their
// in-sandbox agent-server banner, here handed back by the user's script instead.
//
// TLSFingerprint is accepted but IGNORED: af removed TLS, so a fingerprint is
// meaningless now. It stays in the struct only so a launch_cmd written against
// the old TLS contract (which echoed "tls_fingerprint") still parses without
// error — the field is dropped, not honored. New scripts should omit it and echo
// an http:// URL.
type hookEndpointJSON struct {
	URL            string `json:"url"`
	Token          string `json:"token"`
	TLSFingerprint string `json:"tls_fingerprint"`
}

// hookOutputSuffix renders a hook script's combined output for an error message.
// One complete JSON value can be reported with its token fields redacted
// precisely. An unparseable payload is replaced wholesale because none of its
// apparent field boundaries are trustworthy.
//
// Empty output gets named rather than left to inference: "launch_cmd failed:
// exit status 1" with nothing after it reads like af truncated something. Saying
// the script printed nothing points the reader at their script's own error
// handling rather than at af. (The timeout case is carried by the wrapped error
// from runHookScript — "signal: killed" — so it is not re-derived here.)
func hookOutputSuffix(out []byte) string {
	trimmed := strings.TrimSpace(redactHookOutputTokens(string(out)))
	if trimmed == "" {
		return "; it printed nothing — a hook script must report its own errors, " +
			"or the cause reaches nobody (see docs/remote-hooks.md)"
	}
	return fmt.Sprintf("; its output was:\n%s", trimmed)
}

func decodeHookEndpointJSON(candidate string) (hookEndpointJSON, bool) {
	decoder := json.NewDecoder(strings.NewReader(candidate))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return hookEndpointJSON{}, false
	}

	fields := make(map[string]struct{}, 3)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return hookEndpointJSON{}, false
		}
		key, ok := token.(string)
		if !ok {
			return hookEndpointJSON{}, false
		}
		switch key {
		case "url", "token", "tls_fingerprint":
		default:
			return hookEndpointJSON{}, false
		}
		if _, duplicate := fields[key]; duplicate {
			return hookEndpointJSON{}, false
		}
		fields[key] = struct{}{}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return hookEndpointJSON{}, false
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return hookEndpointJSON{}, false
	}
	if _, ok := fields["url"]; !ok {
		return hookEndpointJSON{}, false
	}
	if _, ok := fields["token"]; !ok {
		return hookEndpointJSON{}, false
	}

	var endpoint hookEndpointJSON
	if err := json.Unmarshal([]byte(candidate), &endpoint); err != nil {
		return hookEndpointJSON{}, false
	}
	if strings.TrimSpace(endpoint.URL) == "" || strings.TrimSpace(endpoint.Token) == "" {
		return hookEndpointJSON{}, false
	}
	return endpoint, true
}

// hookStdoutViolation carries the stdout bytes that were not the endpoint
// record. launch_cmd's stdout is reserved for the {"url","token"} JSON, so
// anything else on it breaks the contract — offending is the text quoted back to
// the operator, redacted and bounded by hookStdoutExcerpt before it is shown.
type hookStdoutViolation struct {
	offending string
}

// hookStdoutExcerptRunes bounds the offending text in the error's first line. A
// flooding tunnel must not paste megabytes into a headline whose job is to name
// one thing to go fix; the script's whole output is attached below it anyway.
const hookStdoutExcerptRunes = 200

// hookStdoutExcerptScanBytes bounds what the excerpt reads to find that line.
// The offending text can be the whole of a flooded stdout, and rendering one
// line of it must not cost a redaction pass over all of it.
const hookStdoutExcerptScanBytes = 8 << 10

// parseHookEndpoint reads launch_cmd's stdout, which by contract holds the
// {"url","token"} endpoint record and nothing else. It returns the endpoint,
// whether stdout held JSON at all (which separates "printed nothing" from
// "printed the wrong shape" in the error), and any content that was not the
// record.
//
// This is a parse, not a classification, and that is the whole point of the
// schema change. While docs/remote-hooks.md let a background tunnel inherit
// stdout, "is this line the endpoint or a log?" was UNDECIDABLE: #2845 exhibits
// two input pairs identical on every inspectable property that require opposite
// handling, and seven successive rules each closed one counterexample and
// admitted the next. Reserving the stream removes the question rather than
// answering it — one JSON value, of the documented shape, surrounded by nothing
// but whitespace.
//
// Whitespace is not content: a trailing newline from `echo`, an indent, a blank
// line, and a CRLF ending are all what a shell normally produces and none of
// them is another writer on the stream.
func parseHookEndpoint(stdout string) (*AgentServerEndpoint, bool, *hookStdoutViolation) {
	open := 0
	for open < len(stdout) && isJSONSpace(stdout[open]) {
		open++
	}
	if open >= len(stdout) {
		// Nothing at all on stdout. That is a different diagnosis from "it also
		// printed something else", and gets its own error.
		return nil, false, nil
	}

	decoder := json.NewDecoder(strings.NewReader(stdout[open:]))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		// stdout does not begin with a JSON value, so all of it is the other output
		// the contract forbids — a tunnel that logged first, or a status line.
		return nil, false, &hookStdoutViolation{offending: stdout[open:]}
	}

	valueEnd := open + int(decoder.InputOffset())
	tail := valueEnd
	for tail < len(stdout) && isJSONSpace(stdout[tail]) {
		tail++
	}
	endpoint, isEndpoint := decodeHookEndpointJSON(string(raw))
	if tail >= len(stdout) {
		if isEndpoint {
			// endpoint.TLSFingerprint is intentionally not read — TLS was removed; an
			// old script that still echoes it parses fine and the value is dropped.
			return &AgentServerEndpoint{URL: endpoint.URL, Token: endpoint.Token}, true, nil
		}
		// One value, wrong shape. That is a different diagnosis from a shared
		// stream, and the operator needs to hear about the schema, not a redirect.
		return nil, true, nil
	}

	// More than one thing on stdout, so quote the FIRST of them that is not the
	// endpoint record — the leading value when it is not one, otherwise whatever
	// follows the record. This chooses what to SHOW, never what to dial: both
	// halves are refused either way, so no guess is smuggled back in. A second
	// endpoint-shaped object is refused too, because picking between them is
	// exactly the guess this contract exists to delete.
	if !isEndpoint {
		return nil, true, &hookStdoutViolation{offending: stdout[open:valueEnd]}
	}
	return nil, true, &hookStdoutViolation{offending: stdout[tail:]}
}

// hookStdoutExcerpt renders a safe, bounded summary of non-endpoint stdout for
// the error's first line. A complete JSON value is structurally sanitized; an
// unparseable value collapses to the redaction marker before any line is chosen.
func hookStdoutExcerpt(offending string) string {
	if len(offending) > hookStdoutExcerptScanBytes {
		offending = offending[:hookStdoutExcerptScanBytes]
	}
	for rest := redactHookOutputTokens(offending); rest != ""; {
		line, remainder, _ := strings.Cut(rest, "\n")
		rest = remainder
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if runes := []rune(trimmed); len(runes) > hookStdoutExcerptRunes {
			return strings.TrimRight(string(runes[:hookStdoutExcerptRunes]), " \t") + "…"
		}
		return trimmed
	}
	return ""
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
