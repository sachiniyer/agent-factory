package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const daemonPollIntervalKey = "daemon_poll_interval"

// durationMilliseconds parses a Go duration without losing precision when it
// is stored in daemon_poll_interval's historical integer-millisecond field.
// Sub-millisecond values are rejected rather than silently rounded to zero.
func durationMilliseconds(value string) (int, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, err
	}
	if duration%time.Millisecond != 0 {
		return 0, fmt.Errorf("duration must resolve to whole milliseconds")
	}
	milliseconds := int64(duration / time.Millisecond)
	if int64(int(milliseconds)) != milliseconds {
		return 0, fmt.Errorf("duration is too large for this platform")
	}
	return int(milliseconds), nil
}

// normalizeTOMLDurationValues translates accepted duration strings into the
// historical in-memory integer representation before the typed decoder runs.
// Integer TOML values return the original bytes untouched, so every existing
// config keeps the exact decoder path it had before duration strings existed.
func normalizeTOMLDurationValues(data []byte) ([]byte, error) {
	var document map[string]any
	if err := toml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	raw, present := document[daemonPollIntervalKey]
	if !present {
		return data, nil
	}
	value, isString := raw.(string)
	if !isString {
		return data, nil
	}
	milliseconds, err := durationMilliseconds(value)
	if err != nil {
		return nil, fmt.Errorf("%s must be a Go duration like \"1500ms\" or \"30m\": %w",
			daemonPollIntervalKey, err)
	}
	document[daemonPollIntervalKey] = milliseconds
	return toml.Marshal(document)
}

// normalizeJSONDurationValues gives the frozen legacy JSON reader the same
// additive duration-string support. RawMessage preserves every unrelated JSON
// value while only daemon_poll_interval is normalized for the typed decoder.
func normalizeJSONDurationValues(data []byte) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	raw, present := document[daemonPollIntervalKey]
	if !present {
		return data, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return data, nil
	}
	milliseconds, err := durationMilliseconds(value)
	if err != nil {
		return nil, fmt.Errorf("%s must be a Go duration like \"1500ms\" or \"30m\": %w",
			daemonPollIntervalKey, err)
	}
	document[daemonPollIntervalKey] = json.RawMessage(strconv.Itoa(milliseconds))
	return json.Marshal(document)
}

func validateDaemonPollIntervalValue(value string) error {
	trimmed := strings.TrimSpace(value)
	if milliseconds, err := strconv.Atoi(trimmed); err == nil {
		if milliseconds <= 0 {
			return fmt.Errorf("%s must be positive, got %q", daemonPollIntervalKey, value)
		}
		return nil
	}
	milliseconds, err := durationMilliseconds(trimmed)
	if err != nil {
		return fmt.Errorf("%s must be a Go duration like \"1500ms\" or \"30m\", or a legacy integer number of milliseconds, got %q: %w",
			daemonPollIntervalKey, value, err)
	}
	if milliseconds <= 0 {
		return fmt.Errorf("%s must be positive, got %q", daemonPollIntervalKey, value)
	}
	return nil
}

func canonicalizeDurationScalar(raw string) (canonical, encoded string, err error) {
	trimmed := strings.TrimSpace(raw)
	if milliseconds, parseErr := strconv.Atoi(trimmed); parseErr == nil {
		canonical := strconv.Itoa(milliseconds)
		return canonical, canonical, nil
	}
	if _, parseErr := durationMilliseconds(trimmed); parseErr != nil {
		return "", "", fmt.Errorf("expected a Go duration like \"1500ms\" or \"30m\", or a legacy integer number of milliseconds, got %q: %w", raw, parseErr)
	}
	return trimmed, encodeTOMLString(trimmed), nil
}

// marshalConfigTOML emits the preferred duration spelling for new or
// whole-file config writes while keeping Config's public integer field intact.
// If an unusually large legacy integer exceeds time.Duration's range, it stays
// an integer so saving can never turn a loadable value into an unloadable one.
func marshalConfigTOML(config *Config) ([]byte, error) {
	data, err := toml.Marshal(config)
	if err != nil {
		return nil, err
	}
	durationValue := fmt.Sprintf("%dms", config.DaemonPollInterval)
	if _, err := durationMilliseconds(durationValue); err != nil {
		return data, nil
	}
	integerForm := []byte(fmt.Sprintf("\n%s = %d\n", daemonPollIntervalKey, config.DaemonPollInterval))
	durationForm := []byte(fmt.Sprintf("\n%s = %s\n", daemonPollIntervalKey, encodeTOMLString(durationValue)))
	if bytes.Count(data, integerForm) != 1 {
		return nil, fmt.Errorf("marshaled config does not contain exactly one %s field", daemonPollIntervalKey)
	}
	return bytes.Replace(data, integerForm, durationForm, 1), nil
}
