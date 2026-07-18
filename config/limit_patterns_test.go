package config

import (
	"strings"
	"testing"
	"time"

	aflog "github.com/sachiniyer/agent-factory/log"
)

// TestSanitizeLimitRetryInterval covers the load-path handling of
// limit_retry_interval (#2009): surrounding whitespace on an otherwise-valid
// duration is a hand-edit typo that must be trimmed and honored — NOT thrown
// away for the default — while a genuinely-unparseable non-empty value must
// warn (naming the value and the key) rather than silently fall back.
func TestSanitizeLimitRetryInterval(t *testing.T) {
	const path = "~/.agent-factory/config.toml"

	t.Run("whitespace is trimmed, not defaulted", func(t *testing.T) {
		if got := sanitizeLimitRetryInterval(" 30s ", path); got != "30s" {
			t.Fatalf("sanitizeLimitRetryInterval(%q) = %q, want %q", " 30s ", got, "30s")
		}
	})

	t.Run("empty stays empty (never retry)", func(t *testing.T) {
		if got := sanitizeLimitRetryInterval("", path); got != "" {
			t.Fatalf("sanitizeLimitRetryInterval(%q) = %q, want empty", "", got)
		}
		if got := sanitizeLimitRetryInterval("   ", path); got != "" {
			t.Fatalf("sanitizeLimitRetryInterval(%q) = %q, want empty", "   ", got)
		}
	})

	t.Run("valid value kept verbatim", func(t *testing.T) {
		if got := sanitizeLimitRetryInterval("45m", path); got != "45m" {
			t.Fatalf("sanitizeLimitRetryInterval(%q) = %q, want %q", "45m", got, "45m")
		}
	})

	t.Run("unparseable non-empty value warns rather than silently defaulting", func(t *testing.T) {
		buf := captureLog(t, &aflog.WarningLog)
		got := sanitizeLimitRetryInterval("soon", path)
		if got != defaultLimitRetryInterval {
			t.Fatalf("sanitizeLimitRetryInterval(%q) = %q, want default %q", "soon", got, defaultLimitRetryInterval)
		}
		out := buf.String()
		if !strings.Contains(out, "limit_retry_interval") {
			t.Errorf("warning must name the key, got: %q", out)
		}
		if !strings.Contains(out, "soon") {
			t.Errorf("warning must name the bad value, got: %q", out)
		}
	})

	t.Run("negative value warns rather than silently defaulting", func(t *testing.T) {
		buf := captureLog(t, &aflog.WarningLog)
		got := sanitizeLimitRetryInterval("-5m", path)
		if got != defaultLimitRetryInterval {
			t.Fatalf("sanitizeLimitRetryInterval(%q) = %q, want default %q", "-5m", got, defaultLimitRetryInterval)
		}
		if !strings.Contains(buf.String(), "-5m") {
			t.Errorf("warning must name the bad value, got: %q", buf.String())
		}
	})
}

// TestLimitRetryIntervalDurationTrims pins the runtime consumer's tolerance for
// a whitespace-padded value (#2009): even if an untrimmed value reaches it, it
// must parse to the intended duration, not degrade to 0 (which disables the
// fixed-cadence fallback).
func TestLimitRetryIntervalDurationTrims(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{" 30s ", 30 * time.Second},
		{"30m", 30 * time.Minute},
		{"", 0},
		{"   ", 0},
		{"soon", 0},
	}
	for _, c := range cases {
		cfg := &Config{LimitRetryInterval: c.raw}
		if got := cfg.LimitRetryIntervalDuration(); got != c.want {
			t.Errorf("LimitRetryIntervalDuration(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}
