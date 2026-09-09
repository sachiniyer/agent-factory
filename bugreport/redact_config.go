package bugreport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

type encodedStringScalar struct {
	start int
	end   int
	value string
	shell bool
}

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
	scalars, ok := parseJSONScalars(s, nil)
	if !ok {
		return r.scrubRecognizedText(s, redactionTextUnknown)
	}
	return r.scrubEncodedText(s, redactionTextJSONDocument, scalars)
}

func (r *redactor) scrubConfigText(s string, kind redactionTextKind) string {
	var scalars []encodedStringScalar
	var ok bool
	switch kind {
	case redactionTextConfigJSON:
		scalars, ok = parseJSONConfigScalars(s)
	case redactionTextConfigTOML:
		scalars, ok = parseTOMLConfigScalars(s)
	}
	if !ok {
		return r.scrubRecognizedText(s, redactionTextUnknown)
	}
	return r.scrubEncodedText(s, kind, scalars)
}

func (r *redactor) scrubEncodedText(s string, kind redactionTextKind, scalars []encodedStringScalar) string {
	var spans []redactionSpan
	if kind == redactionTextConfigTOML {
		// TOML comments are free text outside scalar tokens and remain useful in
		// the collected config. JSON has no such region: planning exclusively on
		// its decoded key/value strings guarantees that no replacement can cross
		// structural syntax and make the document unparsable.
		spans = r.genericTextSpans(s)
	}
	for _, scalar := range scalars {
		inner := r.genericTextSpans(scalar.value)
		if scalar.shell {
			inner = append(inner, r.shellCommandPathSpans(scalar.value)...)
		}
		redacted := applyRedactionSpans(scalar.value, inner)
		if redacted == scalar.value {
			continue
		}
		replacement, err := encodeStringForGrammar(redacted, kind)
		if err != nil {
			return r.scrubRecognizedText(s, redactionTextUnknown)
		}
		spans = append(spans, redactionSpan{
			start: scalar.start, end: scalar.end,
			replacement: replacement, priority: spanQuotedValue,
		})
	}
	return applyRedactionSpans(s, spans)
}

// encodeStringForGrammar is the transport half of the recognition invariant:
// a decoded replacement must be valid syntax in its TARGET grammar, never just
// in Go's. In particular, strconv.Quote can emit \xNN escapes which Go accepts
// but JSON rejects. Each encoded-document owner selects its own encoder here;
// adding verbatim exceptions for bytes where two grammars happen to agree would
// only enumerate today's differences.
func encodeStringForGrammar(value string, kind redactionTextKind) (string, error) {
	switch kind {
	case redactionTextJSONDocument, redactionTextConfigJSON:
		return encodeJSONString(value)
	case redactionTextConfigTOML:
		return encodeTOMLString(value)
	default:
		return "", fmt.Errorf("unsupported redaction text grammar %d", kind)
	}
}

func encodeJSONString(value string) (string, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	// Keep legal JSON bytes such as &, <, and > verbatim so redaction does not
	// rewrite content merely because it round-tripped through a decoded scalar.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(encoded.String(), "\n"), nil
}

func encodeTOMLString(value string) (string, error) {
	// The TOML encoder documents that a value containing an apostrophe uses a
	// basic (double-quoted) string. Prefix one solely to select that target-owned
	// grammar, then remove its verbatim byte from the encoded token. This keeps
	// the existing double-quoted report shape without copying TOML's escape table
	// or maintaining a list of bytes on which another grammar happens to agree.
	forced := "'" + value
	doc, err := toml.Marshal(struct {
		Value string `toml:"value"`
	}{Value: forced})
	if err != nil {
		return "", err
	}
	var parser unstable.Parser
	parser.Reset(doc)
	if !parser.NextExpression() {
		if err := parser.Error(); err != nil {
			return "", fmt.Errorf("parse encoded TOML string: %w", err)
		}
		return "", fmt.Errorf("TOML encoder produced no value")
	}
	node := parser.Expression().Value()
	if node == nil || node.Kind != unstable.String {
		return "", fmt.Errorf("TOML encoder produced an unexpected document")
	}
	start := int(node.Raw.Offset)
	end := start + int(node.Raw.Length)
	encoded := string(doc[start:end])
	decoded := string(node.Data)
	if parser.NextExpression() || parser.Error() != nil {
		return "", fmt.Errorf("TOML encoder produced an unexpected document")
	}
	if decoded != forced || len(encoded) < 3 || encoded[0] != '"' || encoded[1] != '\'' {
		return "", fmt.Errorf("TOML encoder did not produce a basic string")
	}
	return encoded[:1] + encoded[2:], nil
}

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

type jsonDocumentParser struct {
	shell   func([]string) bool
	s       string
	pos     int
	scalars []encodedStringScalar
}

func parseJSONConfigScalars(s string) ([]encodedStringScalar, bool) {
	return parseJSONScalars(s, isConfigShellPath)
}

func parseJSONScalars(s string, shell func([]string) bool) ([]encodedStringScalar, bool) {
	if !json.Valid([]byte(s)) {
		return nil, false
	}
	p := jsonDocumentParser{s: s, shell: shell}
	if !p.parseValue(nil) {
		return nil, false
	}
	p.skipSpace()
	return p.scalars, p.pos == len(s)
}

func (p *jsonDocumentParser) parseValue(path []string) bool {
	p.skipSpace()
	if p.pos >= len(p.s) {
		return false
	}
	switch p.s[p.pos] {
	case '{':
		return p.parseObject(path)
	case '[':
		return p.parseArray(path)
	case '"':
		scalar, ok := p.parseString()
		if ok {
			if p.shell != nil {
				scalar.shell = p.shell(path)
			}
			p.scalars = append(p.scalars, scalar)
		}
		return ok
	default:
		start := p.pos
		for p.pos < len(p.s) && !strings.ContainsRune(",]}", rune(p.s[p.pos])) && !unicode.IsSpace(rune(p.s[p.pos])) {
			p.pos++
		}
		return p.pos > start
	}
}

func (p *jsonDocumentParser) parseObject(path []string) bool {
	p.pos++
	p.skipSpace()
	if p.take('}') {
		return true
	}
	for {
		key, ok := p.parseString()
		if !ok {
			return false
		}
		p.scalars = append(p.scalars, key)
		p.skipSpace()
		if !p.take(':') || !p.parseValue(appendConfigPath(path, key.value)) {
			return false
		}
		p.skipSpace()
		if p.take('}') {
			return true
		}
		if !p.take(',') {
			return false
		}
		p.skipSpace()
	}
}

func (p *jsonDocumentParser) parseArray(path []string) bool {
	p.pos++
	p.skipSpace()
	if p.take(']') {
		return true
	}
	for {
		if !p.parseValue(path) {
			return false
		}
		p.skipSpace()
		if p.take(']') {
			return true
		}
		if !p.take(',') {
			return false
		}
	}
}

func (p *jsonDocumentParser) parseString() (encodedStringScalar, bool) {
	if p.pos >= len(p.s) || p.s[p.pos] != '"' {
		return encodedStringScalar{}, false
	}
	start := p.pos
	end := goQuotedEnd(p.s, start)
	if end < 0 {
		return encodedStringScalar{}, false
	}
	var value string
	if err := json.Unmarshal([]byte(p.s[start:end]), &value); err != nil {
		return encodedStringScalar{}, false
	}
	p.pos = end
	return encodedStringScalar{start: start, end: end, value: value}, true
}

func (p *jsonDocumentParser) skipSpace() {
	for p.pos < len(p.s) && unicode.IsSpace(rune(p.s[p.pos])) {
		p.pos++
	}
}

func (p *jsonDocumentParser) take(want byte) bool {
	if p.pos >= len(p.s) || p.s[p.pos] != want {
		return false
	}
	p.pos++
	return true
}

func parseTOMLConfigScalars(s string) ([]encodedStringScalar, bool) {
	data := []byte(s)
	offset := 0
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		offset = 3
		data = data[offset:]
	}
	var parser unstable.Parser
	parser.Reset(data)
	var scalars []encodedStringScalar
	var tablePath []string
	for parser.NextExpression() {
		expression := parser.Expression()
		switch expression.Kind {
		case unstable.Table, unstable.ArrayTable:
			tablePath = nil
			tablePath, scalars = appendTOMLKeys(tablePath, scalars, expression.Key(), offset)
		case unstable.KeyValue:
			scalars = appendTOMLKeyValueScalars(scalars, expression, tablePath, offset)
		}
	}
	return scalars, parser.Error() == nil
}

func appendTOMLKeyValueScalars(
	scalars []encodedStringScalar,
	node *unstable.Node,
	base []string,
	offset int,
) []encodedStringScalar {
	path, scalars := appendTOMLKeys(base, scalars, node.Key(), offset)
	return appendTOMLValueScalars(scalars, node.Value(), path, offset)
}

func appendTOMLKeys(
	path []string,
	scalars []encodedStringScalar,
	keys unstable.Iterator,
	offset int,
) ([]string, []encodedStringScalar) {
	path = appendConfigPath(nil, path...)
	for keys.Next() {
		key := keys.Node()
		start := offset + int(key.Raw.Offset)
		scalars = append(scalars, encodedStringScalar{
			start: start, end: start + int(key.Raw.Length), value: string(key.Data),
		})
		path = append(path, string(key.Data))
	}
	return path, scalars
}

func appendTOMLValueScalars(
	scalars []encodedStringScalar,
	node *unstable.Node,
	path []string,
	offset int,
) []encodedStringScalar {
	if node == nil {
		return scalars
	}
	switch node.Kind {
	case unstable.KeyValue:
		return appendTOMLKeyValueScalars(scalars, node, path, offset)
	case unstable.String:
		start := offset + int(node.Raw.Offset)
		scalars = append(scalars, encodedStringScalar{
			start: start, end: start + int(node.Raw.Length), value: string(node.Data), shell: isConfigShellPath(path),
		})
		return scalars
	}
	children := node.Children()
	for children.Next() {
		scalars = appendTOMLValueScalars(scalars, children.Node(), path, offset)
	}
	return scalars
}

func appendConfigPath(path []string, values ...string) []string {
	out := make([]string, 0, len(path)+len(values))
	out = append(out, path...)
	return append(out, values...)
}
