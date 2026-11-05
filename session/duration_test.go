package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ParseAgeDuration is one parser behind both age flags (#6770): everything
// time.ParseDuration accepts keeps its exact value, and the day/week units a
// retention window is written in translate to plain hours.
func TestParseAgeDuration(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"720h", 720 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"30d", 30 * 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"1w2d", 9 * 24 * time.Hour},
		{"2d12h", 60 * time.Hour},
		{"1.5d", 36 * time.Hour},
		{"0", 0},
		{"-7d", -7 * 24 * time.Hour},
		{"90m", 90 * time.Minute},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseAgeDuration(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseAgeDuration_Rejects(t *testing.T) {
	for _, in := range []string{
		"", "not-a-duration", "d", "7", "7dd", "7D", "1mo", "1y",
		"7d ", " 7d", "1.2.3d", "99999999999999999999d",
	} {
		t.Run(in, func(t *testing.T) {
			_, err := ParseAgeDuration(in)
			require.Error(t, err)
		})
	}
}
