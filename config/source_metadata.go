package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/pelletier/go-toml/v2"
)

// sourceMetadata is the presence-preserving half of a decoded config file.
// The typed structs hold validated values; shape records which top-level keys
// and nested leaves were actually present in that source. Keeping the two on
// the same loaded value prevents provenance from being reconstructed by a
// second, racy read after defaults have already been overlaid.
type sourceMetadata struct {
	path   string
	format ConfigFormat
	shape  map[string]any

	// builtIn is the exact defaults snapshot onto which the global file was
	// decoded. It prevents the resolver from probing machine-dependent defaults
	// a second time and calling that later probe the source of the loaded value.
	builtIn *Config
}

// metadataForSource decodes a file into its shapeless representation after the
// real typed decoder has accepted it. It is deliberately not a second source
// of validation or values: callers use it only for presence and leaf names.
func metadataForSource(data []byte, path string, format ConfigFormat) (sourceMetadata, error) {
	shape := make(map[string]any)
	switch format {
	case FormatTOML:
		if err := toml.Unmarshal(data, &shape); err != nil {
			return sourceMetadata{}, err
		}
	case FormatJSON:
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&shape); err != nil {
			return sourceMetadata{}, err
		}
	default:
		return sourceMetadata{}, fmt.Errorf("unsupported config format %s", format)
	}
	return sourceMetadata{path: path, format: format, shape: shape}, nil
}

func (m sourceMetadata) topLevel(key string) (any, bool) {
	if m.shape == nil {
		return nil, false
	}
	value, present := m.shape[key]
	return value, present
}

func attachConfigSource(cfg *Config, data []byte, path string, format ConfigFormat) error {
	builtIn := cfg.source.builtIn
	metadata, err := metadataForSource(data, path, format)
	if err != nil {
		return err
	}
	metadata.builtIn = builtIn
	cfg.source = metadata
	return nil
}

// snapshotConfig copies every exported value recursively and deliberately
// drops loader-only metadata. The reflection walk means a future map, slice,
// pointer, or table default cannot accidentally alias the value that decoding
// mutates; adding such a field needs no second hand-maintained copy list.
func snapshotConfig(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	copyValue := cloneExportedValue(reflect.ValueOf(*cfg))
	copyConfig := copyValue.Interface().(Config)
	return &copyConfig
}

func cloneExportedValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return reflect.Value{}
	}
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type().Elem())
		cloned.Elem().Set(cloneExportedValue(value.Elem()))
		return cloned
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneExportedValue(value.Elem())
		out := reflect.New(value.Type()).Elem()
		out.Set(cloned)
		return out
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			cloned.SetMapIndex(cloneExportedValue(iter.Key()), cloneExportedValue(iter.Value()))
		}
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			cloned.Index(i).Set(cloneExportedValue(value.Index(i)))
		}
		return cloned
	case reflect.Struct:
		cloned := reflect.New(value.Type()).Elem()
		typeOf := value.Type()
		for i := 0; i < value.NumField(); i++ {
			if !typeOf.Field(i).IsExported() {
				continue
			}
			cloned.Field(i).Set(cloneExportedValue(value.Field(i)))
		}
		return cloned
	default:
		return value
	}
}

func parseLoadedConfigTOML(data []byte, prettyPath, path string) (*Config, error) {
	cfg, err := parseConfigTOML(data, prettyPath)
	if err != nil {
		return nil, err
	}
	if err := attachConfigSource(cfg, data, path, FormatTOML); err != nil {
		return nil, fmt.Errorf("failed to record config presence for %s: %w", prettyPath, err)
	}
	return cfg, nil
}

func parseLoadedConfigJSON(data []byte, prettyPath, path string) (*Config, error) {
	cfg, err := parseConfig(data, prettyPath)
	if err != nil {
		return nil, err
	}
	if err := attachConfigSource(cfg, data, path, FormatJSON); err != nil {
		return nil, fmt.Errorf("failed to record config presence for %s: %w", prettyPath, err)
	}
	return cfg, nil
}
