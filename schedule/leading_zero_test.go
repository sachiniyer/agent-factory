package schedule

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCronEveryNHoursAcceptsLeadingZeroMinute(t *testing.T) {
	got, ok := ParseCron("00 */2 * * *")

	require.True(t, ok, "a numeric zero minute must select the every-N-hours preset")
	require.Equal(t, Schedule{Type: EveryNHours, Interval: 2}, got)
}

func TestParseCronEveryNHoursRejectsNonzeroMinute(t *testing.T) {
	for _, expr := range []string{"01 */2 * * *", "60 */2 * * *"} {
		t.Run(expr, func(t *testing.T) {
			got, ok := ParseCron(expr)

			require.False(t, ok)
			require.Equal(t, Schedule{Type: Custom, Raw: expr}, got)
		})
	}
}
