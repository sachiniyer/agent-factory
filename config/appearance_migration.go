package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
	"github.com/sachiniyer/agent-factory/log"
)

// RetiredThemeKeyError is shared by CLI and daemon-backed editor operations.
func RetiredThemeKeyError(key string) error {
	if key == "theme" || strings.HasPrefix(key, "theme.") {
		return fmt.Errorf("config key %q is retired; use `af config set appearance light|dark|system` (custom colors are no longer supported)", key)
	}
	return nil
}

func migratedAppearance(shape map[string]any) string {
	if value, ok := shape["appearance"].(string); ok {
		return NormalizeAppearance(value)
	}
	if value, ok := shape["theme"].(string); ok && (value == "light" || value == "dark") {
		return value
	}
	return "system"
}

func appearanceMigrationKeys(shape map[string]any) []string {
	var keys []string
	if _, ok := shape["theme"]; ok {
		keys = append(keys, "theme")
	}
	if value, ok := shape["appearance"]; ok && value == "auto" {
		keys = append(keys, "appearance=auto")
	}
	return keys
}

// Only LoadConfig calls this persistence boundary. Shared parsers and diagnostics
// perform the same value mapping in memory without creating locks or writing.
func persistAppearanceMigration(cfg *Config) (*Config, error) {
	if len(appearanceMigrationKeys(cfg.source.shape)) == 0 {
		return cfg, nil
	}
	path := cfg.source.path
	prettyPath := prettyHomePath(path)
	var result *Config
	err := withFollowedFileLock(path, func(locked lockedTarget) error {
		current, err := locked.read()
		if err != nil {
			return err
		}
		// Re-read and validate inside the lock: another writer may have won it.
		result, err = parseLoadedConfigTOML(current, prettyPath, path)
		if err != nil {
			return err
		}
		metadata, err := metadataForSource(current, prettyPath, FormatTOML)
		if err != nil {
			return err
		}
		keys := appearanceMigrationKeys(metadata.shape)
		if len(keys) == 0 {
			return locked.confirm()
		}
		updated, err := retireThemeTOML(current, result.Appearance)
		if err != nil {
			return err
		}
		result, err = parseLoadedConfigTOML(updated, prettyPath, path)
		if err != nil {
			return err
		}
		perm, err := locked.perm()
		if err != nil {
			return err
		}
		if err = locked.write(updated, perm); err != nil {
			return err
		}
		warnAppearanceMigration(prettyPath, keys, result.Appearance)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("migrate appearance in %s: %w", prettyPath, err)
	}
	return result, nil
}

func warnAppearanceMigration(path string, keys []string, appearance string) {
	if len(keys) > 0 {
		log.WarningLog.Printf("config %s: migrated retired %s to appearance = %q; use `af config set appearance light|dark|system`", path, strings.Join(keys, ", "), appearance)
	}
}

// Remove only theme expressions, leaving all unrelated bytes and comments in
// place. Parser ranges distinguish real keys from text inside multiline strings.
func retireThemeTOML(original []byte, appearance string) ([]byte, error) {
	data := stripUTF8BOM(original)
	newline := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		newline = "\r\n"
	}
	type edit struct {
		start, end  int
		replacement string
	}
	var edits []edit
	var parser unstable.Parser
	parser.KeepComments = true
	parser.Reset(data)
	var table []string
	hasAppearance := false
	for parser.NextExpression() {
		expr := parser.Expression()
		parts, offset := tomlExpressionKey(expr)
		switch expr.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = parts
			if len(table) > 0 && table[0] == "theme" {
				start := bytes.LastIndexByte(data[:offset], '\n') + 1
				end := offset
				it := expr.Key()
				for it.Next() {
					n := it.Node()
					end = int(n.Raw.Offset + n.Raw.Length)
				}
				for end < len(data) && data[end] != ']' {
					end++
				}
				end++
				if expr.Kind == unstable.ArrayTable {
					end++
				}
				edits = append(edits, edit{start: start, end: end})
			}
		case unstable.KeyValue:
			retired := (len(table) > 0 && table[0] == "theme") || (len(table) == 0 && len(parts) > 0 && parts[0] == "theme")
			if retired {
				// Retain actual comments inside a multiline legacy array/inline table,
				// while discarding strings that merely contain comment-looking text.
				var comments []string
				var collect func(*unstable.Node)
				collect = func(n *unstable.Node) {
					if n.Kind == unstable.Comment {
						comments = append(comments, string(parser.Raw(n.Raw)))
						return
					}
					it := n.Children()
					for it.Next() {
						collect(it.Node())
					}
				}
				collect(expr)
				edits = append(edits, edit{start: int(expr.Raw.Offset), end: int(expr.Raw.Offset + expr.Raw.Length), replacement: strings.Join(comments, newline)})
			} else if len(table) == 0 && len(parts) == 1 && parts[0] == "appearance" {
				hasAppearance = true
				v := expr.Value()
				edits = append(edits, edit{start: int(v.Raw.Offset), end: int(v.Raw.Offset + v.Raw.Length), replacement: encodeTOMLString(appearance)})
			}
		}
	}
	if err := parser.Error(); err != nil {
		return nil, err
	}
	// Build into a fresh buffer: source metadata and validation still refer to
	// the original bytes, and no line-based editor may reinterpret CRLF headers.
	var out bytes.Buffer
	if len(original) != len(data) {
		out.WriteString("\xef\xbb\xbf")
	}
	if !hasAppearance {
		out.WriteString("appearance = " + encodeTOMLString(appearance) + newline)
	}
	cursor := 0
	for _, e := range edits {
		out.Write(data[cursor:e.start])
		out.WriteString(e.replacement)
		cursor = e.end
	}
	out.Write(data[cursor:])
	before, err := metadataForSource(original, "migration input", FormatTOML)
	if err != nil {
		return nil, err
	}
	after, err := metadataForSource(out.Bytes(), "migration output", FormatTOML)
	if err != nil {
		return nil, err
	}
	delete(before.shape, "theme")
	before.shape["appearance"] = appearance
	if !reflect.DeepEqual(before.shape, after.shape) {
		return nil, fmt.Errorf("appearance migration would change unrelated config values; no changes written")
	}
	return out.Bytes(), nil
}

// JSON has no comments. Preserve unknown values without activating fields that
// the frozen JSON reader ignores (notably keys and canonical network settings).
func preserveJSONUnknownTOML(data []byte, shape map[string]any) ([]byte, error) {
	var generated map[string]any
	if err := toml.Unmarshal(data, &generated); err != nil {
		return nil, err
	}
	defaults := DefaultConfig()
	var merge func(map[string]any, map[string]any, string) error
	merge = func(dst, src map[string]any, prefix string) error {
		for key, value := range src {
			if prefix == "" && (key == "theme" || key == "appearance") {
				continue
			}
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			field, known := configFieldByTomlKey(defaults, path)
			if !known && prefix != "" {
				if parent, ok := configFieldByTomlKey(defaults, prefix); ok {
					field, known = taggedFieldByKey(parent, key)
				}
			}
			if known && field.Kind() != reflect.Struct {
				// The typed conversion owns known settings, including deliberate
				// omissions of TOML-only maps such as keys. Structs still recurse
				// so unknown members survive without replacing known fields.
				continue
			}
			if table, ok := value.(map[string]any); ok {
				target, ok := dst[key].(map[string]any)
				if !ok {
					target = map[string]any{}
				}
				if err := merge(target, table, path); err != nil {
					return err
				}
				if len(target) > 0 {
					dst[key] = target
				}
			} else if _, exists := dst[key]; !exists {
				normalized, err := jsonValueForTOML(value)
				if err != nil {
					return err
				}
				dst[key] = normalized
			}
		}
		return nil
	}
	if err := merge(generated, shape, ""); err != nil {
		return nil, err
	}
	return toml.Marshal(generated)
}

func jsonValueForTOML(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			return strconv.ParseFloat(string(v), 64)
		}
		return strconv.ParseInt(string(v), 10, 64)
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			n, err := jsonValueForTOML(item)
			if err != nil {
				return nil, err
			}
			result[i] = n
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(v))
		for k, item := range v {
			n, err := jsonValueForTOML(item)
			if err != nil {
				return nil, err
			}
			result[k] = n
		}
		return result, nil
	case nil:
		return nil, fmt.Errorf("cannot preserve a JSON null in TOML; original config.json has not been changed")
	default:
		return value, nil
	}
}
