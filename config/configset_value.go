package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/keys"
)

// canonicalizeConfigValue selects the scalar syntax used by existing CLI keys
// or the compact-JSON whole-value syntax used by table/list rows in the panes.
func canonicalizeConfigValue(key string, spec settableKeySpec, structured bool, raw string) (canonical, encoded string, err error) {
	if !structured {
		return canonicalizeScalar(spec.kind, raw)
	}
	if key == "theme" && !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		var preset ThemeConfig
		if err := preset.UnmarshalText([]byte(raw)); err != nil {
			return "", "", err
		}
		return preset.Preset(), "theme = " + encodeTOMLString(preset.Preset()) + "\n", nil
	}
	return canonicalizeStructuredValue(key, raw)
}

// canonicalizeStructuredValue decodes one pane value into the real Config field
// type, applies the same eager validation expected at an `af config set`
// boundary, and emits a targeted TOML replacement. CurrentValue is the inverse:
// its compact JSON is accepted here byte-for-byte for every structured row.
func canonicalizeStructuredValue(key, raw string) (canonical, encoded string, err error) {
	return canonicalizeStructuredValueAgainst(key, raw, nil, false)
}

// canonicalizeStructuredValueAgainst is the global writer's locked variant.
// A partial theme object overlays the palette that is current inside the file
// lock, never an unrelated preset. Whole program-overrides maps preserve an
// omitted auto-detected default as an empty TOML tombstone so the loader cannot
// seed that command back in after the user removed it.
func canonicalizeStructuredValueAgainst(key, raw string, current *Config, preserveBuiltInRemovals bool) (canonical, encoded string, err error) {
	if strings.TrimSpace(raw) == "null" {
		return "", "", fmt.Errorf("expected compact JSON for %s, got null", key)
	}
	holder := &Config{}
	if key == "theme" {
		holder.Theme = DefaultThemeConfig()
		if current != nil {
			holder.Theme = current.Theme
		}
	}
	field, ok := writableConfigFieldByTomlKey(holder, key)
	if !ok {
		return "", "", fmt.Errorf("%q does not name a writable global config field", key)
	}
	if key == "root_agent" {
		var decoded rootAgentConfigJSON
		if err := decodeCompactJSON(key, raw, &decoded); err != nil {
			return "", "", err
		}
		if err := rejectStructuredNulls(key, raw); err != nil {
			return "", "", err
		}
		value := reflect.ValueOf(decoded)
		encoded, err := encodeStructuredTOML(key, value)
		if err != nil {
			return "", "", err
		}
		return editorValue(value), encoded, nil
	}

	target := field.Addr().Interface()
	var decodedTheme themeConfigJSON
	if key == "theme" {
		// ThemeConfig implements encoding.TextUnmarshaler for preset strings.
		// Decode custom JSON through a method-free alias so an object retains its
		// established field-by-field shape instead of being routed to that scalar
		// hook and rejected.
		decodedTheme = themeConfigJSON(holder.Theme)
		target = &decodedTheme
	}
	if err := decodeCompactJSON(key, raw, target); err != nil {
		return "", "", err
	}
	if err := rejectStructuredNulls(key, raw); err != nil {
		return "", "", err
	}
	if key == "theme" {
		field.Set(reflect.ValueOf(ThemeConfig(decodedTheme)))
	}
	if err := validateStructuredConfigValue(key, field); err != nil {
		return "", "", err
	}

	canonical = editorValue(field)
	encodedValue := field
	if key == "program_overrides" && preserveBuiltInRemovals {
		requested := field.Interface().(map[string]string)
		onDisk := make(map[string]string, len(requested)+1)
		for agent, command := range requested {
			onDisk[agent] = command
		}
		// Empty entries are removal tombstones hidden from CurrentValue. Keep
		// them even if the corresponding executable is temporarily absent from
		// PATH while another override is edited; otherwise the executable would
		// silently reappear the next time detection succeeds.
		if current != nil {
			for agent, command := range current.ProgramOverrides {
				if command == "" {
					if _, present := requested[agent]; !present {
						onDisk[agent] = ""
					}
				}
			}
		}
		for agent := range DefaultConfig().ProgramOverrides {
			if _, present := requested[agent]; !present {
				onDisk[agent] = ""
			}
		}
		encodedValue = reflect.ValueOf(onDisk)
	}
	encoded, err = encodeStructuredTOML(key, encodedValue)
	if err != nil {
		return "", "", err
	}
	return canonical, encoded, nil
}

type themeConfigJSON ThemeConfig

// rootAgentConfigJSON preserves JSON field presence while a structured value is
// encoded. RootAgent.Enabled deliberately lacks omitempty for full Config
// serialization, but a personal override that omits enabled must keep inheriting
// that field from lower layers rather than materializing enabled=false.
type rootAgentConfigJSON struct {
	Enabled *bool   `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Program *string `json:"program,omitempty" toml:"program,omitempty"`
}

func decodeCompactJSON(key, raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("expected compact JSON for %s: %w", key, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("expected one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

// rejectStructuredNulls rejects explicit JSON nulls at any depth. TOML has no
// null value; allowing encoding/json to quietly retain a preseeded default (or
// a Go zero value) would turn an invalid edit into a different valid setting.
func rejectStructuredNulls(key, raw string) error {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil // decodeCompactJSON already returns the actionable syntax error.
	}
	if path, ok := firstJSONNullPath(value, key); ok {
		return fmt.Errorf("%s must not be null", path)
	}
	return nil
}

func firstJSONNullPath(value any, path string) (string, bool) {
	switch typed := value.(type) {
	case nil:
		return path, true
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if found, ok := firstJSONNullPath(typed[key], path+"."+key); ok {
				return found, true
			}
		}
	case []any:
		for i, item := range typed {
			if found, ok := firstJSONNullPath(item, fmt.Sprintf("%s[%d]", path, i)); ok {
				return found, true
			}
		}
	}
	return "", false
}

func writableConfigFieldByTomlKey(cfg *Config, key string) (reflect.Value, bool) {
	rv := reflect.ValueOf(cfg)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return reflect.Value{}, false
	}
	rv = rv.Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		field := rt.Field(i)
		if field.IsExported() && structTagName(field.Tag.Get("toml")) == key {
			return rv.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func validateStructuredConfigValue(key string, field reflect.Value) error {
	switch key {
	case "theme":
		for i := range field.NumField() {
			if !field.Type().Field(i).IsExported() {
				continue
			}
			raw := strings.TrimSpace(field.Field(i).String())
			name := structTagName(field.Type().Field(i).Tag.Get("toml"))
			if !themeHexColorRE.MatchString(raw) {
				return fmt.Errorf("theme.%s must be a #RRGGBB color, got %q", name, field.Field(i).String())
			}
			field.Field(i).SetString("#" + strings.ToUpper(raw[1:]))
		}
	case "program_overrides":
		for agent, command := range field.Interface().(map[string]string) {
			if err := ValidateProgramEnum("program_overrides key", "program_overrides key", agent, command); err != nil {
				return err
			}
		}
	case "session_env_passthrough":
		normalized, err := sessionenv.NormalizeExtraNames(field.Interface().([]string))
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(normalized))
	case "limit_patterns":
		for agent, pattern := range field.Interface().(map[string]string) {
			if err := ValidateProgramEnum("limit_patterns key", "limit_patterns key", agent, pattern); err != nil {
				return err
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("limit_patterns.%s is not a valid regular expression: %w", agent, err)
			}
		}
	case "keys":
		overrides, err := normalizeKeyOverrides(field.Interface().(map[string]any), "af config set keys")
		if err != nil {
			return err
		}
		if err := keys.ValidateOverrides(overrides); err != nil {
			return err
		}
	}
	return nil
}

// encodeStructuredTOML emits a complete targeted definition. Lists remain a
// root key; maps and structs become ordinary TOML tables so the convenient
// dynamic leaf writer can still update program_overrides.<agent> and
// limit_patterns.<agent> after a whole-map pane save.
func encodeStructuredTOML(key string, value reflect.Value) (string, error) {
	value = unwrapTOMLValue(value)
	if value.Kind() == reflect.Slice || value.Kind() == reflect.Array {
		encoded, err := encodeTOMLValue(value)
		if err != nil {
			return "", err
		}
		return key + " = " + encoded + "\n", nil
	}

	entries, err := encodeTOMLTableEntries(value)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", key)
	for _, entry := range entries {
		b.WriteString(entry)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func encodeTOMLTableEntries(value reflect.Value) ([]string, error) {
	value = unwrapTOMLValue(value)
	var entries []string
	switch value.Kind() {
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("expected a JSON object with string keys")
		}
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, key := range keys {
			encoded, err := encodeTOMLValue(value.MapIndex(key))
			if err != nil {
				return nil, err
			}
			entries = append(entries, encodeTOMLKey(key.String())+" = "+encoded)
		}
	case reflect.Struct:
		for i := range value.NumField() {
			typeField := value.Type().Field(i)
			name, omitEmpty, skip := tomlFieldName(typeField)
			if skip || (omitEmpty && value.Field(i).IsZero()) {
				continue
			}
			encoded, err := encodeTOMLValue(value.Field(i))
			if err != nil {
				return nil, err
			}
			entries = append(entries, encodeTOMLKey(name)+" = "+encoded)
		}
	default:
		return nil, fmt.Errorf("expected a JSON object, got %s", value.Kind())
	}
	return entries, nil
}

func encodeTOMLValue(value reflect.Value) (string, error) {
	value = unwrapTOMLValue(value)
	if !value.IsValid() {
		return "", fmt.Errorf("TOML has no null value")
	}
	switch value.Kind() {
	case reflect.String:
		return encodeTOMLString(value.String()), nil
	case reflect.Bool:
		return strconv.FormatBool(value.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(value.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(value.Float(), 'g', -1, value.Type().Bits()), nil
	case reflect.Slice, reflect.Array:
		parts := make([]string, value.Len())
		for i := range value.Len() {
			encoded, err := encodeTOMLValue(value.Index(i))
			if err != nil {
				return "", err
			}
			parts[i] = encoded
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case reflect.Map, reflect.Struct:
		entries, err := encodeTOMLTableEntries(value)
		if err != nil {
			return "", err
		}
		return "{ " + strings.Join(entries, ", ") + " }", nil
	default:
		return "", fmt.Errorf("unsupported JSON value type %s", value.Kind())
	}
}

func unwrapTOMLValue(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

func tomlFieldName(field reflect.StructField) (name string, omitEmpty, skip bool) {
	tag := field.Tag.Get("toml")
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "-" || !field.IsExported() {
		return "", false, true
	}
	if name == "" {
		name = field.Name
	}
	for _, option := range parts[1:] {
		omitEmpty = omitEmpty || option == "omitempty"
	}
	return name, omitEmpty, false
}

func encodeTOMLKey(key string) string {
	if key != "" {
		bare := true
		for i := 0; i < len(key); i++ {
			c := key[i]
			if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				bare = false
				break
			}
		}
		if bare {
			return key
		}
	}
	return encodeTOMLString(key)
}

// canonicalizeScalar parses rawValue per kind and returns both the canonical
// string form (for echo/validation) and its TOML encoding (for the file).
func canonicalizeScalar(kind cfgValueKind, raw string) (canonical, encoded string, err error) {
	switch kind {
	case cfgBool:
		b, perr := strconv.ParseBool(strings.TrimSpace(raw))
		if perr != nil {
			return "", "", fmt.Errorf("expected a boolean (true/false), got %q", raw)
		}
		s := strconv.FormatBool(b)
		return s, s, nil
	case cfgInt:
		n, perr := strconv.Atoi(strings.TrimSpace(raw))
		if perr != nil {
			return "", "", fmt.Errorf("expected an integer, got %q", raw)
		}
		s := strconv.Itoa(n)
		return s, s, nil
	case cfgDuration:
		return canonicalizeDurationScalar(raw)
	case cfgStringList:
		elems := splitListValue(raw)
		encoded := "[]"
		if len(elems) > 0 {
			quoted := make([]string, len(elems))
			for i, e := range elems {
				quoted[i] = encodeTOMLString(e)
			}
			encoded = "[" + strings.Join(quoted, ", ") + "]"
		}
		// Canonical is the comma-joined trimmed elements — exactly the form the
		// editor shows and `af config get` prints back, so a set→get round-trips.
		return strings.Join(elems, ","), encoded, nil
	default:
		return raw, encodeTOMLString(raw), nil
	}
}

// encodeTOMLString renders s through the TOML library rather than manually
// quoting user input. Marshal cannot fail for a string-only envelope; a panic
// here would therefore indicate a broken dependency contract, not bad input.
func encodeTOMLString(s string) string {
	type envelope struct {
		Value string `toml:"value"`
	}
	encoded, err := toml.Marshal(envelope{Value: s})
	if err != nil {
		panic(fmt.Sprintf("encode TOML string: %v", err))
	}
	line := strings.TrimSuffix(string(encoded), "\n")
	const prefix = "value = "
	if !strings.HasPrefix(line, prefix) || strings.Contains(line[len(prefix):], "\n") {
		panic(fmt.Sprintf("unexpected TOML string encoding %q", line))
	}
	return strings.TrimPrefix(line, prefix)
}

type byteRange struct {
	start int
	end   int
}

// setTOMLStructured replaces every existing spelling of one top-level table or
// list and inserts its canonical definition. The unstable parser is used only
// for source ranges: unlike line regexes it sees a multiline array or inline
// table as one expression, so replacing session_env_passthrough cannot strand
// half of the old value in the file. Unrelated expressions stay byte-identical.
func setTOMLStructured(content, key, definition string) (string, error) {
	cleaned, err := removeTOMLTopLevelValue(content, key)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(definition, "[") {
		cleaned = strings.TrimRight(cleaned, "\n")
		if cleaned != "" {
			cleaned += "\n\n"
		}
		return cleaned + definition, nil
	}
	prefix := key + " = "
	if !strings.HasPrefix(definition, prefix) {
		return "", fmt.Errorf("invalid root definition for %s", key)
	}
	return setTOMLScalar(cleaned, "", key, strings.TrimSuffix(strings.TrimPrefix(definition, prefix), "\n")), nil
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
			currentTable = parts
			currentBlock = &tableBlock{start: start, target: len(parts) > 0 && parts[0] == key}
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
