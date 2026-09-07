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
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 11, 0, 0, 0, loc)
	cases := []struct {
		zones []string
		want  string
		warn  bool
	}{
		{[]string{"UTC"}, "2026-09-07T17:00:00Z", false},
		{[]string{"GMT"}, "2026-09-07T17:00:00Z", false},
		{[]string{"Zulu"}, "2026-09-07T17:00:00Z", false},
		{[]string{"Japan"}, "2026-09-08T08:00:00Z", false},
		{[]string{"Israel"}, "2026-09-08T14:00:00Z", false},
		{[]string{"Singapore"}, "2026-09-08T09:00:00Z", false},
		{[]string{"Turkey"}, "2026-09-08T14:00:00Z", false},
		{[]string{"EST"}, "2026-09-07T22:00:00Z", false},
		{[]string{"America/New_York"}, "2026-09-07T21:00:00Z", false},
		{[]string{"Etc/UTC"}, "2026-09-07T17:00:00Z", false},
		{[]string{"Etc/GMT+5"}, "2026-09-07T22:00:00Z", false},
		{[]string{"Etc/GMT-9"}, "2026-09-08T08:00:00Z", false},
		{[]string{"XYZ"}, "2026-09-07T21:00:00Z", true},
		{[]string{"weekly"}, "2026-09-07T21:00:00Z", true},
		{[]string{"5h limit"}, "2026-09-07T21:00:00Z", true},
		{[]string{"Pro", "America/Los_Angeles"}, "2026-09-08T00:00:00Z", false},
		{[]string{"UTC", "extra"}, "2026-09-07T17:00:00Z", false},
		{[]string{"Pro", "Team"}, "2026-09-07T21:00:00Z", true},
		{nil, "2026-09-07T21:00:00Z", false},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.zones, ","), func(t *testing.T) {
			var warnings bytes.Buffer
			previous := log.WarningLog.Writer()
			log.WarningLog.SetOutput(&warnings)
			t.Cleanup(func() { log.WarningLog.SetOutput(previous) })
			banner := "Claude usage limit reached. Your limit will reset at 5pm"
			for _, zone := range tc.zones {
				banner += " (" + zone + ")"
			}
			hit, reset, parsed := NewLimitDetector(nil).Check(banner, tmux.ProgramClaude, now)
			if !hit || !parsed || reset.Format(time.RFC3339) != tc.want {
				t.Errorf("hit=%v parsed=%v reset=%s; want %s", hit, parsed, reset.Format(time.RFC3339), tc.want)
			}
			if tc.warn {
				if strings.Count(warnings.String(), "\n") != 1 || !strings.Contains(warnings.String(), banner) || !strings.Contains(warnings.String(), loc.String()) {
					t.Errorf("want one warning naming banner and fallback zone; got %q", warnings.String())
				}
				if !strings.Contains(warnings.String(), fmt.Sprintf("timezone candidates %q", tc.zones)) {
					t.Errorf("warning must list all rejected candidates: %s", &warnings)
				}
			} else if warnings.Len() != 0 {
				t.Errorf("unexpected warning: %s", &warnings)
			}
		})
	}
}
