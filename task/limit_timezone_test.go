package task

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestClaudeResetParenTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, loc)
	cases := []struct {
		zone, want string
		warn       bool
	}{
		{"UTC", "2026-09-07T17:00:00Z", false},
		{"GMT", "2026-09-07T17:00:00Z", false},
		{"Zulu", "2026-09-07T17:00:00Z", false},
		{"Japan", "2026-09-08T08:00:00Z", false},
		{"Israel", "2026-09-08T14:00:00Z", false},
		{"Singapore", "2026-09-08T09:00:00Z", false},
		{"Turkey", "2026-09-08T14:00:00Z", false},
		{"EST", "2026-09-07T22:00:00Z", false},
		{"America/New_York", "2026-09-07T21:00:00Z", false},
		{"Etc/UTC", "2026-09-07T17:00:00Z", false},
		{"Etc/GMT+5", "2026-09-07T22:00:00Z", false},
		{"Etc/GMT-9", "2026-09-08T08:00:00Z", false},
		{"XYZ", "2026-09-08T00:00:00Z", true},
		{"weekly", "2026-09-08T00:00:00Z", true},
		{"5h limit", "2026-09-08T00:00:00Z", true},
		{"", "2026-09-08T00:00:00Z", false},
	}
	for _, tc := range cases {
		t.Run(tc.zone, func(t *testing.T) {
			var warnings bytes.Buffer
			previous := log.WarningLog.Writer()
			log.WarningLog.SetOutput(&warnings)
			t.Cleanup(func() { log.WarningLog.SetOutput(previous) })
			banner := "Claude usage limit reached. Your limit will reset at 5pm"
			if tc.zone != "" {
				banner += fmt.Sprintf(" (%s)", tc.zone)
			}
			hit, reset, parsed := NewLimitDetector(nil).Check(banner, tmux.ProgramClaude, now)
			if !hit || !parsed || reset.Format(time.RFC3339) != tc.want {
				t.Errorf("hit=%v parsed=%v reset=%s; want %s", hit, parsed, reset.Format(time.RFC3339), tc.want)
			}
			if tc.warn {
				if strings.Count(warnings.String(), "\n") != 1 || !strings.Contains(warnings.String(), banner) || !strings.Contains(warnings.String(), tc.zone) || !strings.Contains(warnings.String(), loc.String()) {
					t.Errorf("want one warning naming banner and fallback zone; got %q", warnings.String())
				}
			} else if warnings.Len() != 0 {
				t.Errorf("unexpected warning: %s", &warnings)
			}
		})
	}
}
