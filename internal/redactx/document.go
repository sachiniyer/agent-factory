package redactx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// Scalar is one string token inside a parser-proven document: its raw source
// range, its decoded value, and whether the document's schema proves it a
// shell command. A scalar's replacement must be re-encoded into the owning
// grammar — that is what the Encode*String functions are for.
type Scalar struct {
	Start int
	End   int
	Value string
	Shell bool
}

// ParseJSONScalars decodes every string scalar in a JSON document — object
// keys included, since a key can be user-authored text — and reports false
// when the document is not valid JSON. shell, when non-nil, is the consumer's
// schema predicate: it receives the key path of each VALUE scalar and marks
// the ones the schema declares shell commands. Key scalars never get it —
// a key is syntax that happens to decode, not a command field.
func ParseJSONScalars(s string, shell func([]string) bool) ([]Scalar, bool) {
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

// ParseTOMLScalars decodes every string scalar in a TOML document and returns
// the comment payload ranges beside them: TOML comments are parser-owned free
// text outside scalar tokens, exposed WITHOUT their leading '#' so every
// replacement stays a valid comment. Reports false when the document does not
// parse — callers fail closed over the whole document, never best-effort.
func ParseTOMLScalars(s string, shell func([]string) bool) ([]Scalar, []Range, bool) {
	data := []byte(s)
	offset := 0
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		offset = 3
		data = data[offset:]
	}
	parser := unstable.Parser{KeepComments: true}
	parser.Reset(data)
	var scalars []Scalar
	var comments []Range
	var tablePath []string
	for parser.NextExpression() {
		expression := parser.Expression()
		comments = appendTOMLCommentRanges(comments, expression, offset)
		// A comment on the expression's own line attaches as its root's next
		// sibling — it is not yielded as an expression and not a child, so
		// without this step a credential in a trailing comment ships verbatim.
		if next := expression.Next(); next != nil && next.Kind == unstable.Comment {
			comments = appendTOMLCommentRanges(comments, next, offset)
		}
		switch expression.Kind {
		case unstable.Table, unstable.ArrayTable:
			tablePath = nil
			tablePath, scalars = appendTOMLKeys(tablePath, scalars, expression.Key(), offset)
		case unstable.KeyValue:
			scalars = appendTOMLKeyValueScalars(scalars, expression, tablePath, offset, shell)
		}
	}
	return scalars, comments, parser.Error() == nil
}

func appendTOMLCommentRanges(comments []Range, node *unstable.Node, offset int) []Range {
	if node == nil {
		return comments
	}
	if node.Kind == unstable.Comment {
		start := offset + int(node.Raw.Offset)
		end := start + int(node.Raw.Length)
		if start+1 < end {
			comments = append(comments, Range{Start: start + 1, End: end})
		}
		return comments
	}
	children := node.Children()
	for children.Next() {
		comments = appendTOMLCommentRanges(comments, children.Node(), offset)
	}
	return comments
}

func appendTOMLKeyValueScalars(
	scalars []Scalar,
	node *unstable.Node,
	base []string,
	offset int,
	shell func([]string) bool,
) []Scalar {
	path, scalars := appendTOMLKeys(base, scalars, node.Key(), offset)
	return appendTOMLValueScalars(scalars, node.Value(), path, offset, shell)
}

func appendTOMLKeys(
	path []string,
	scalars []Scalar,
	keys unstable.Iterator,
	offset int,
) ([]string, []Scalar) {
	path = appendConfigPath(nil, path...)
	for keys.Next() {
		key := keys.Node()
		start := offset + int(key.Raw.Offset)
		scalars = append(scalars, Scalar{
			Start: start, End: start + int(key.Raw.Length), Value: string(key.Data),
		})
		path = append(path, string(key.Data))
	}
	return path, scalars
}

func appendTOMLValueScalars(
	scalars []Scalar,
	node *unstable.Node,
	path []string,
	offset int,
	shell func([]string) bool,
) []Scalar {
	if node == nil {
		return scalars
	}
	switch node.Kind {
	case unstable.KeyValue:
		return appendTOMLKeyValueScalars(scalars, node, path, offset, shell)
	case unstable.String:
		start := offset + int(node.Raw.Offset)
		scalars = append(scalars, Scalar{
			Start: start, End: start + int(node.Raw.Length),
			Value: string(node.Data), Shell: shell != nil && shell(path),
		})
		return scalars
	}
	children := node.Children()
	for children.Next() {
		scalars = appendTOMLValueScalars(scalars, children.Node(), path, offset, shell)
	}
	return scalars
}

func appendConfigPath(path []string, values ...string) []string {
	out := make([]string, 0, len(path)+len(values))
	out = append(out, path...)
	return append(out, values...)
}

type jsonDocumentParser struct {
	shell   func([]string) bool
	s       string
	pos     int
	scalars []Scalar
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
				scalar.Shell = p.shell(path)
			}
			p.scalars = append(p.scalars, scalar)
		}
		return ok
	default:
		start := p.pos
		for p.pos < len(p.s) &&
			!strings.ContainsRune(",]}", rune(p.s[p.pos])) &&
			!unicode.IsSpace(rune(p.s[p.pos])) {
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
		if !p.take(':') || !p.parseValue(appendConfigPath(path, key.Value)) {
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

func (p *jsonDocumentParser) parseString() (Scalar, bool) {
	if p.pos >= len(p.s) || p.s[p.pos] != '"' {
		return Scalar{}, false
	}
	start := p.pos
	end := GoQuotedEnd(p.s, start)
	if end < 0 {
		return Scalar{}, false
	}
	var value string
	if err := json.Unmarshal([]byte(p.s[start:end]), &value); err != nil {
		return Scalar{}, false
	}
	p.pos = end
	return Scalar{Start: start, End: end, Value: value}, true
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

// EncodeJSONString is the transport half of the recognition invariant: a
// decoded replacement must be valid syntax in its TARGET grammar, never just
// in Go's. In particular, strconv.Quote can emit \xNN escapes which Go accepts
// but JSON rejects.
func EncodeJSONString(value string) (string, error) {
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

// EncodeTOMLString re-encodes a decoded value into a TOML basic string. The
// TOML encoder documents that a value containing an apostrophe uses a basic
// (double-quoted) string; prefix one solely to select that target-owned
// grammar, then remove its verbatim byte from the encoded token. This keeps
// the existing double-quoted report shape without copying TOML's escape table
// or maintaining a list of bytes on which another grammar happens to agree.
func EncodeTOMLString(value string) (string, error) {
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
