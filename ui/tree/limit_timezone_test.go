package tree

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestLimitBadgeParsedTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	oldLocal := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = oldLocal })
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, loc)
	for _, tc := range []struct{ zone, badge string }{
		{"UTC", "[limit] resets 10am "},
		{"Japan", "[limit] resets Sep 8 1am "},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			hit, reset, parsed := task.NewLimitDetector(nil).Check("Claude usage limit reached. Your limit will reset at 5pm ("+tc.zone+")", "claude", now)
			require.True(t, hit)
			require.True(t, parsed)
			inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "claude"})
			require.NoError(t, err)
			inst.SetLimitReached(reset)
			require.Equal(t, tc.badge, limitBadgePrefix(inst, now))
		})
	}
}
