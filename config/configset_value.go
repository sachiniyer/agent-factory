package config

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/keys"
)

// canonicalizeConfigValue selects the scalar syntax used by existing CLI keys
// or the compact-JSON whole-value syntax used by table/list rows in the panes.
func canonicalizeConfigValue(key string, spec settableKeySpec, structured bool, raw string) (canonical, encoded string, err error) {
	if !structured {
		return canonicalizeScalar(spec.kind, raw)
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

// canonicalizeStructuredValueAgainst is the writers' locked variant. existing
// is the program_overrides map already on the layer being written: a whole-map
// write that omits an agent keeps that agent's "" tombstone, so replacing the
// table cannot bring back the lower layer's override (#4699). reseedBuiltIns is
// global-only: it also writes a tombstone for every omitted auto-detected
// default so the loader cannot seed that command back in after the user
// removed it. A personal project layer preserves only its own tombstones.
func canonicalizeStructuredValueAgainst(key, raw string, existing map[string]string, reseedBuiltIns bool) (canonical, encoded string, err error) {
	if strings.TrimSpace(raw) == "null" {
		return "", "", fmt.Errorf("expected compact JSON for %s, got null", key)
	}
	holder := &Config{}
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

	// account_handoff gets the same sparse-write treatment: the wrapper's
	// pointer leaves keep an omitted member absent on disk so the field-wise
	// merge (preserveStructuredMembers) and lower layers still own it. Each
	// supplied leaf is validated with the loader's own rules up front, so a
	// write that would fail the post-edit re-parse is refused here with the
	// leaf named.
	if key == "account_handoff" {
		var decoded accountHandoffConfigJSON
		if err := decodeCompactJSON(key, raw, &decoded); err != nil {
			return "", "", err
		}
		if err := rejectStructuredNulls(key, raw); err != nil {
			return "", "", err
		}
		if err := validateAccountHandoffSparse(decoded); err != nil {
			return "", "", err
		}
		value := reflect.ValueOf(decoded)
		encoded, err := encodeStructuredTOML(key, value)
		if err != nil {
			return "", "", err
		}
		return editorValue(value), encoded, nil
	}

	// A whole-table write may omit a leaf — the loader supplies the compiled
	// default for an absent one, so the typed form must seed the same defaults
	// before decoding: a zero value left by an omitted member would read as a
	// range violation below. An explicitly-written member still decodes over
	// its seed, so a present-but-invalid value fails as strictly as the leaf
	// form does.
	if key == "web" {
		field.Set(reflect.ValueOf(DefaultConfig().Web))
	}
	target := field.Addr().Interface()
	if err := decodeCompactJSON(key, raw, target); err != nil {
		return "", "", err
	}
	if err := rejectStructuredNulls(key, raw); err != nil {
		return "", "", err
	}
	if err := validateStructuredConfigValue(key, field); err != nil {
		return "", "", err
	}

	canonical = editorValue(field)
	encodedValue := field
	if key == "program_overrides" && (len(existing) > 0 || reseedBuiltIns) {
		requested := field.Interface().(map[string]string)
		onDisk := make(map[string]string, len(requested)+1)
		for agent, command := range requested {
			onDisk[agent] = command
		}
		// Empty entries are removal tombstones hidden from CurrentValue. Keep
		// them even if the corresponding executable is temporarily absent from
		// PATH while another override is edited; otherwise the executable would
		// silently reappear the next time detection succeeds.
		for agent, command := range existing {
			if command == "" {
				if _, present := requested[agent]; !present {
					onDisk[agent] = ""
				}
			}
		}
		if reseedBuiltIns {
			for agent := range DefaultConfig().ProgramOverrides {
				if _, present := requested[agent]; !present {
					onDisk[agent] = ""
				}
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

// rootAgentConfigJSON preserves JSON field presence while a structured value is
// encoded. RootAgent.Enabled deliberately lacks omitempty for full Config
// serialization, but a personal override that omits enabled must keep inheriting
// that field from lower layers rather than materializing enabled=false.
type rootAgentConfigJSON struct {
	Enabled *bool   `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Program *string `json:"program,omitempty" toml:"program,omitempty"`
}

// accountHandoffConfigJSON is the sparse-write mirror of AccountHandoffConfig:
// every leaf is a pointer so a whole-table JSON write only materializes the
// members it actually names. That is the write-side twin of the manifest's
// MergeTableByField — an omitted leaf must keep inheriting the stored value
// (and, on the personal layer, the global leaf) rather than being reset to a
// zero value or silently re-defaulted. An explicitly supplied zero/false/empty
// member stays a real write (the pointer is non-nil), which is how
// move_idle_at_percent = 0, include_pinned = false, or exclude_sessions = []
// is told apart from an omitted leaf.
type accountHandoffConfigJSON struct {
	Mode               *string   `json:"mode,omitempty" toml:"mode,omitempty"`
	IncludePinned      *bool     `json:"include_pinned,omitempty" toml:"include_pinned,omitempty"`
	TargetOrder        *string   `json:"target_order,omitempty" toml:"target_order,omitempty"`
	ConfirmWall        *bool     `json:"confirm_wall,omitempty" toml:"confirm_wall,omitempty"`
	WallPercent        *int      `json:"wall_percent,omitempty" toml:"wall_percent,omitempty"`
	MoveIdleAtPercent  *int      `json:"move_idle_at_percent,omitempty" toml:"move_idle_at_percent,omitempty"`
	PreferBelowPercent *int      `json:"prefer_below_percent,omitempty" toml:"prefer_below_percent,omitempty"`
	ProbeTTL           *string   `json:"probe_ttl,omitempty" toml:"probe_ttl,omitempty"`
	DefaultLogin       *string   `json:"default_login,omitempty" toml:"default_login,omitempty"`
	RequireCarry       *bool     `json:"require_carry,omitempty" toml:"require_carry,omitempty"`
	ExcludeSessions    *[]string `json:"exclude_sessions,omitempty" toml:"exclude_sessions,omitempty"`
	MaxMoves           *int      `json:"max_moves,omitempty" toml:"max_moves,omitempty"`
	MaxMovesWindow     *string   `json:"max_moves_window,omitempty" toml:"max_moves_window,omitempty"`
	RefusedBackoff     *string   `json:"refused_backoff,omitempty" toml:"refused_backoff,omitempty"`
	OnFailure          *string   `json:"on_failure,omitempty" toml:"on_failure,omitempty"`
	ReportTo           *string   `json:"report_to,omitempty" toml:"report_to,omitempty"`
}

// validateAccountHandoffSparse applies the loader's per-leaf rules to the
// members a structured write actually supplied — the early, leaf-named refusal
// for a value the merged-file re-parse would reject anyway.
func validateAccountHandoffSparse(d accountHandoffConfigJSON) error {
	checks := []struct {
		leaf    string
		strVal  *string
		intVal  *int
		listVal *[]string
	}{
		{"mode", d.Mode, nil, nil},
		{"target_order", d.TargetOrder, nil, nil},
		{"default_login", d.DefaultLogin, nil, nil},
		{"on_failure", d.OnFailure, nil, nil},
		{"probe_ttl", d.ProbeTTL, nil, nil},
		{"max_moves_window", d.MaxMovesWindow, nil, nil},
		{"refused_backoff", d.RefusedBackoff, nil, nil},
		{"wall_percent", nil, d.WallPercent, nil},
		{"move_idle_at_percent", nil, d.MoveIdleAtPercent, nil},
		{"prefer_below_percent", nil, d.PreferBelowPercent, nil},
		{"max_moves", nil, d.MaxMoves, nil},
		{"exclude_sessions", nil, nil, d.ExcludeSessions},
	}
	for _, check := range checks {
		switch {
		case check.strVal != nil:
			if err := validateAccountHandoffLeaf(check.leaf, *check.strVal); err != nil {
				return err
			}
		case check.intVal != nil:
			if err := validateAccountHandoffLeaf(check.leaf, strconv.Itoa(*check.intVal)); err != nil {
				return err
			}
		case check.listVal != nil:
			for i, title := range *check.listVal {
				if strings.TrimSpace(title) == "" {
					return fmt.Errorf("account_handoff.exclude_sessions[%d] must not be empty", i)
				}
			}
		}
	}
	return nil
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
	case "sandbox_agent_config":
		normalized, err := NormalizeSandboxAgentConfig(field.Interface().([]string))
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(normalized))
	case "default_accounts":
		for agent, name := range field.Interface().(map[string]string) {
			if err := ValidateDefaultAccountEntry("default_accounts key", agent, name); err != nil {
				return err
			}
		}
	case "limit_patterns":
		for agent, pattern := range field.Interface().(map[string]string) {
			if err := ValidateProgramEnum("limit_patterns key", "limit_patterns key", agent, pattern); err != nil {
				return err
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("limit_patterns.%s is not a valid regular expression: %w", agent, err)
			}
		}
	case "hosts":
		for name, entry := range field.Interface().(map[string]HostConfig) {
			if err := ValidateHostEntry(name, entry); err != nil {
				return err
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
	case "web":
		// The whole-table JSON form posts both leaves at once; each gets the
		// same range check the leaf writer runs, under the leaf's own key name
		// so a refusal names the field that was wrong.
		web := field.Interface().(WebConfig)
		for leaf, value := range map[string]float64{
			"terminal_font_size_phone":   web.TerminalFontSizePhone,
			"terminal_font_size_desktop": web.TerminalFontSizeDesktop,
		} {
			if err := validateWebTerminalFontSize(leaf, strconv.FormatFloat(value, 'g', -1, 64)); err != nil {
				return err
			}
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
	case cfgFloat:
		n, perr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if perr != nil {
			return "", "", fmt.Errorf("expected a number, got %q", raw)
		}
		canonical = strconv.FormatFloat(n, 'g', -1, 64)
		// The FILE spelling needs a fraction or exponent — a bare "8" would
		// decode as a TOML integer, which a float64 field refuses at load.
		// The canonical echo keeps the short "8" `config get` renders.
		encoded = canonical
		if !strings.ContainsAny(canonical, ".eE") && !math.IsNaN(n) && !math.IsInf(n, 0) {
			encoded += ".0"
		}
		return canonical, encoded, nil
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
