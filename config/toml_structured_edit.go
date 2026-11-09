package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// The structured-value half of the `af config set` surgical edit: where
// toml_scalar_edit.go rewrites one scalar's line, these functions replace a
// whole top-level table or list definition in the document text using the
// unstable parser's source ranges, carry members this binary does not know
// from the old table into its replacement, and read the stored form back for
// the panes. The value layer that produces the replacement definition lives in
// configset_value.go.

type byteRange struct {
	start int
	end   int
}

// setTOMLStructured replaces every existing spelling of one top-level table or
// list and inserts its canonical definition. The unstable parser is used only
// for source ranges: unlike line regexes it sees a multiline array or inline
// table as one expression, so replacing session_env_passthrough cannot strand
// half of the old value in the file. Unrelated expressions stay byte-identical.
// root_agent is a field-wise patch: omitted profile members are retained.
func setTOMLStructured(content, key, definition string) (string, error) {
	definition, err := preserveStructuredMembers(content, key, definition)
	if err != nil {
		return "", err
	}
	cleaned, err := removeTOMLTopLevelValue(content, key)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(definition, "[") {
		body := strings.TrimRight(cleaned, "\n")
		trailing := cleaned[len(body):]
		if trailing == "" {
			trailing = "\n"
		}
		if body != "" {
			body += "\n\n"
		}
		return body + strings.TrimRight(definition, "\n") + trailing, nil
	}
	prefix := key + " = "
	if !strings.HasPrefix(definition, prefix) {
		return "", fmt.Errorf("invalid root definition for %s", key)
	}
	return setTOMLScalar(cleaned, "", key, strings.TrimSuffix(strings.TrimPrefix(definition, prefix), "\n")), nil
}

// canonicalStructuredTOMLValue returns the compact JSON form of the value
// actually stored in a structured TOML definition. This is the read-back form
// used by config set after a field-wise merge.
func canonicalStructuredTOMLValue(content, key string) (string, error) {
	var document map[string]any
	if err := toml.Unmarshal([]byte(content), &document); err != nil {
		return "", err
	}
	value, ok := document[key]
	if !ok {
		return "", fmt.Errorf("stored structured value %s is missing", key)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// projectStructuredCurrentValue returns the typed, writer-accepted form of a
// structured personal-project value. root_agent and account_handoff need their
// source shape as well as their decoded value: enabled=false, mode="off", and
// move_idle_at_percent=0 are meaningful when explicitly present, while an
// omitted field must keep inheriting from the global layer.
func projectStructuredCurrentValue(cfg *ProjectConfig, key string) (string, bool) {
	field, ok := taggedFieldByKey(reflect.ValueOf(cfg), key)
	if !ok {
		return "", false
	}
	if key == "account_handoff" {
		shapeValue, _ := cfg.source.topLevel(key)
		shape, ok := shapeValue.(map[string]any)
		if !ok {
			return "", false
		}
		return editorValue(accountHandoffSparseCurrentValue(field, shape)), true
	}
	if key != "root_agent" {
		if key == "program_overrides" {
			visible := make(map[string]string, field.Len())
			iter := field.MapRange()
			for iter.Next() {
				if command := iter.Value().String(); command != "" {
					visible[iter.Key().String()] = command
				}
			}
			return editorValue(reflect.ValueOf(visible)), true
		}
		return editorValue(field), true
	}
	shapeValue, _ := cfg.source.topLevel(key)
	shape, ok := shapeValue.(map[string]any)
	if !ok {
		return "", false
	}
	var value rootAgentConfigJSON
	if _, present := shape["enabled"]; present {
		enabled := cfg.RootAgent.Enabled
		value.Enabled = &enabled
	}
	if _, present := shape["program"]; present {
		program := cfg.RootAgent.Program
		value.Program = &program
	}
	return editorValue(reflect.ValueOf(value)), true
}

// accountHandoffSparseCurrentValue renders only the [account_handoff] leaves a
// source shape actually wrote: an absent table shows {}, a sparse table stays
// sparse, and an explicit zero/false/"" leaf keeps its written value. Used by
// both CurrentValue (global) and projectStructuredCurrentValue (personal), so
// the two surfaces describe the same table the same way.
//
// It renders through the sparse-write mirror rather than a map so a FULL table
// emits the exact bytes the structural fallback (editorValue on
// AccountHandoffConfig) does — same field order, same [] for an empty list —
// which is what TestCurrentValueRoundTripsThroughConfigSet's set-canonicalized-
// to-shown assertion pins.
func accountHandoffSparseCurrentValue(field reflect.Value, shape map[string]any) reflect.Value {
	rt := reflect.TypeOf(AccountHandoffConfig{})
	out := reflect.ValueOf(&accountHandoffConfigJSON{}).Elem()
	for i := range rt.NumField() {
		name := structTagName(rt.Field(i).Tag.Get("toml"))
		if name == "" || name == "-" {
			continue
		}
		if _, present := shape[name]; !present {
			continue
		}
		// The mirror declares the same Go field names, one pointer per leaf.
		target := out.FieldByName(rt.Field(i).Name)
		if !target.IsValid() || !target.CanSet() {
			continue
		}
		ptr := reflect.New(field.Field(i).Type())
		ptr.Elem().Set(field.Field(i))
		target.Set(ptr)
	}
	return out
}

// preserveStructuredMembers retains omitted root_agent profile fields and
// carries fields this binary does not know
// from the old target table into its replacement. The loader deliberately
// ignores such fields for rollback tolerance; dropping them during an edit of a
// known sibling would make that tolerance destructive. Dynamic map keys remain
// ordinary user values, so omission still removes them. For other typed structs
// (including structs stored in maps), only unknown members are retained.
func preserveStructuredMembers(content, key, definition string) (string, error) {
	var existingDoc, replacementDoc map[string]any
	if err := toml.Unmarshal([]byte(content), &existingDoc); err != nil {
		return "", err
	}
	if err := toml.Unmarshal([]byte(definition), &replacementDoc); err != nil {
		return "", err
	}
	existing, exists := existingDoc[key]
	replacement, replaces := replacementDoc[key]
	if !exists || !replaces {
		return definition, nil
	}

	field, ok := writableConfigFieldByTomlKey(&Config{}, key)
	if !ok {
		return definition, nil
	}
	var merged any
	var changed bool
	if key == "root_agent" || key == "account_handoff" {
		previous, oldOK := existing.(map[string]any)
		next, newOK := replacement.(map[string]any)
		if !oldOK || !newOK {
			return "", fmt.Errorf("%s must be a table", key)
		}
		for member, value := range previous {
			if _, supplied := next[member]; !supplied {
				next[member] = value
				changed = true
			}
		}
		merged = next
	} else {
		var err error
		merged, changed, err = mergeUnknownStructuredMembers(existing, replacement, field.Type(), key)
		if err != nil {
			return "", err
		}
	}
	if !changed {
		return definition, nil
	}
	encoded, err := toml.Marshal(map[string]any{key: merged})
	if err != nil {
		return "", fmt.Errorf("preserve unknown members of %s: %w", key, err)
	}
	return string(encoded), nil
}

func mergeUnknownStructuredMembers(existing, replacement any, schema reflect.Type, path string) (any, bool, error) {
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	switch schema.Kind() {
	case reflect.Struct:
		existingMap, existingIsMap := existing.(map[string]any)
		if !existingIsMap {
			return replacement, false, nil
		}
		replacementMap, replacementIsMap := replacement.(map[string]any)
		if !replacementIsMap {
			if unknownPath, ok := firstUnknownStructuredMember(existingMap, schema, path); ok {
				return nil, false, fmt.Errorf("cannot replace %s while preserving newer config member %s; update af or remove that member manually", path, unknownPath)
			}
			return replacement, false, nil
		}
		known := tomlStructFieldTypes(schema)
		changed := false
		for name, oldValue := range existingMap {
			fieldType, isKnown := known[name]
			if !isKnown {
				if _, alreadyPresent := replacementMap[name]; !alreadyPresent {
					replacementMap[name] = oldValue
					changed = true
				}
				continue
			}
			newValue, stillPresent := replacementMap[name]
			if !stillPresent {
				continue
			}
			merged, nestedChanged, err := mergeUnknownStructuredMembers(oldValue, newValue, fieldType, path+"."+name)
			if err != nil {
				return nil, false, err
			}
			if nestedChanged {
				replacementMap[name] = merged
				changed = true
			}
		}
		return replacementMap, changed, nil
	case reflect.Map:
		existingMap, existingIsMap := existing.(map[string]any)
		replacementMap, replacementIsMap := replacement.(map[string]any)
		if !existingIsMap || !replacementIsMap || schema.Key().Kind() != reflect.String {
			return replacement, false, nil
		}
		changed := false
		for name, newValue := range replacementMap {
			oldValue, existed := existingMap[name]
			if !existed {
				continue
			}
			merged, nestedChanged, err := mergeUnknownStructuredMembers(oldValue, newValue, schema.Elem(), path+"."+name)
			if err != nil {
				return nil, false, err
			}
			if nestedChanged {
				replacementMap[name] = merged
				changed = true
			}
		}
		return replacementMap, changed, nil
	default:
		return replacement, false, nil
	}
}

func firstUnknownStructuredMember(value map[string]any, schema reflect.Type, path string) (string, bool) {
	known := tomlStructFieldTypes(schema)
	for name, nested := range value {
		fieldType, ok := known[name]
		if !ok {
			return path + "." + name, true
		}
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct {
			if nestedMap, ok := nested.(map[string]any); ok {
				if unknown, found := firstUnknownStructuredMember(nestedMap, fieldType, path+"."+name); found {
					return unknown, true
				}
			}
		}
	}
	return "", false
}

func tomlStructFieldTypes(schema reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	for i := range schema.NumField() {
		field := schema.Field(i)
		name, _, skip := tomlFieldName(field)
		if !skip {
			fields[name] = field.Type
		}
	}
	return fields
}

func removeTOMLTopLevelValue(content, key string) (string, error) {
	data := []byte(content)
	if len(data) == 0 {
		return content, nil
	}

	type tableBlock struct {
		start  int
		target bool
	}
	var (
		parser       unstable.Parser
		currentTable []string
		currentBlock *tableBlock
		ranges       []byteRange
	)
	parser.Reset(data)
	for parser.NextExpression() {
		expr := parser.Expression()
		parts, firstKeyOffset := tomlExpressionKey(expr)
		switch expr.Kind {
		case unstable.Table, unstable.ArrayTable:
			start, _ := wholeLineRange(data, firstKeyOffset, firstKeyOffset)
			if currentBlock != nil && currentBlock.target {
				end := leadingTableCommentStart(data, start, currentBlock.start)
				ranges = append(ranges, byteRange{start: currentBlock.start, end: end})
			}
			isTarget := len(parts) > 0 && parts[0] == key
			blockStart := start
			if isTarget && currentBlock != nil {
				blockStart = leadingTableCommentStart(data, start, currentBlock.start)
			}
			currentTable = parts
			currentBlock = &tableBlock{start: blockStart, target: isTarget}
		case unstable.KeyValue:
			if len(currentTable) == 0 && len(parts) > 0 && parts[0] == key {
				raw := expr.Raw
				start, end := wholeLineRange(data, int(raw.Offset), int(raw.Offset+raw.Length))
				ranges = append(ranges, byteRange{start: start, end: end})
			}
		}
	}
	if err := parser.Error(); err != nil {
		return "", err
	}
	if currentBlock != nil && currentBlock.target {
		ranges = append(ranges, byteRange{start: currentBlock.start, end: len(data)})
	}
	if len(ranges) == 0 {
		return content, nil
	}

	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start > ranges[j].start })
	for _, r := range ranges {
		data = append(data[:r.start], data[r.end:]...)
	}
	return string(data), nil
}

func tomlExpressionKey(expr *unstable.Node) (parts []string, firstOffset int) {
	firstOffset = -1
	if expr == nil || (expr.Kind != unstable.Table && expr.Kind != unstable.ArrayTable && expr.Kind != unstable.KeyValue) {
		return nil, firstOffset
	}
	iter := expr.Key()
	for iter.Next() {
		node := iter.Node()
		if firstOffset < 0 {
			firstOffset = int(node.Raw.Offset)
		}
		parts = append(parts, string(node.Data))
	}
	return parts, firstOffset
}

func wholeLineRange(data []byte, start, end int) (int, int) {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if previous := bytes.LastIndexByte(data[:start], '\n'); previous >= 0 {
		start = previous + 1
	} else {
		start = 0
	}
	if end < len(data) {
		if next := bytes.IndexByte(data[end:], '\n'); next >= 0 {
			end += next + 1
		} else {
			end = len(data)
		}
	}
	return start, end
}

// leadingTableCommentStart returns the start of the contiguous comment block
// immediately introducing a table header. Those comments belong to the next
// table, not to the target table being replaced. A blank line stops the scan,
// so comments in the target table remain part of the target block.
func leadingTableCommentStart(data []byte, headerStart, floor int) int {
	start := headerStart
	for start > floor {
		lineEnd := start
		if lineEnd > 0 && data[lineEnd-1] == '\n' {
			lineEnd--
		}
		lineStart := floor
		if previous := bytes.LastIndexByte(data[floor:lineEnd], '\n'); previous >= 0 {
			lineStart = floor + previous + 1
		}
		line := bytes.TrimSpace(data[lineStart:lineEnd])
		if !bytes.HasPrefix(line, []byte("#")) {
			break
		}
		start = lineStart
	}
	return start
}
