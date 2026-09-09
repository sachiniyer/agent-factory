package bugreport

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2/unstable"
)

type configStringScalar struct {
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

func (r *redactor) scrubConfigText(s string, kind redactionTextKind) string {
	var scalars []configStringScalar
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
	spans := r.genericTextSpans(s)
	for _, scalar := range scalars {
		inner := r.genericTextSpans(scalar.value)
		if scalar.shell {
			inner = append(inner, r.shellCommandPathSpans(scalar.value)...)
		}
		redacted := applyRedactionSpans(scalar.value, inner)
		if redacted == scalar.value {
			continue
		}
		replacement, err := encodeConfigString(redacted)
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

func encodeConfigString(value string) (string, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(encoded.String(), "\n"), nil
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

type jsonConfigParser struct {
	s       string
	pos     int
	scalars []configStringScalar
}

func parseJSONConfigScalars(s string) ([]configStringScalar, bool) {
	if !json.Valid([]byte(s)) {
		return nil, false
	}
	p := jsonConfigParser{s: s}
	if !p.parseValue(nil) {
		return nil, false
	}
	p.skipSpace()
	return p.scalars, p.pos == len(s)
}

func (p *jsonConfigParser) parseValue(path []string) bool {
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
			scalar.shell = isConfigShellPath(path)
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

func (p *jsonConfigParser) parseObject(path []string) bool {
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

func (p *jsonConfigParser) parseArray(path []string) bool {
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

func (p *jsonConfigParser) parseString() (configStringScalar, bool) {
	if p.pos >= len(p.s) || p.s[p.pos] != '"' {
		return configStringScalar{}, false
	}
	start := p.pos
	end := goQuotedEnd(p.s, start)
	if end < 0 {
		return configStringScalar{}, false
	}
	var value string
	if err := json.Unmarshal([]byte(p.s[start:end]), &value); err != nil {
		return configStringScalar{}, false
	}
	p.pos = end
	return configStringScalar{start: start, end: end, value: value}, true
}

func (p *jsonConfigParser) skipSpace() {
	for p.pos < len(p.s) && unicode.IsSpace(rune(p.s[p.pos])) {
		p.pos++
	}
}

func (p *jsonConfigParser) take(want byte) bool {
	if p.pos >= len(p.s) || p.s[p.pos] != want {
		return false
	}
	p.pos++
	return true
}

func parseTOMLConfigScalars(s string) ([]configStringScalar, bool) {
	data := []byte(s)
	offset := 0
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		offset = 3
		data = data[offset:]
	}
	var parser unstable.Parser
	parser.Reset(data)
	var scalars []configStringScalar
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
	scalars []configStringScalar,
	node *unstable.Node,
	base []string,
	offset int,
) []configStringScalar {
	path, scalars := appendTOMLKeys(base, scalars, node.Key(), offset)
	return appendTOMLValueScalars(scalars, node.Value(), path, offset)
}

func appendTOMLKeys(
	path []string,
	scalars []configStringScalar,
	keys unstable.Iterator,
	offset int,
) ([]string, []configStringScalar) {
	path = appendConfigPath(nil, path...)
	for keys.Next() {
		key := keys.Node()
		start := offset + int(key.Raw.Offset)
		scalars = append(scalars, configStringScalar{
			start: start, end: start + int(key.Raw.Length), value: string(key.Data),
		})
		path = append(path, string(key.Data))
	}
	return path, scalars
}

func appendTOMLValueScalars(
	scalars []configStringScalar,
	node *unstable.Node,
	path []string,
	offset int,
) []configStringScalar {
	if node == nil {
		return scalars
	}
	switch node.Kind {
	case unstable.KeyValue:
		return appendTOMLKeyValueScalars(scalars, node, path, offset)
	case unstable.String:
		start := offset + int(node.Raw.Offset)
		scalars = append(scalars, configStringScalar{
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
