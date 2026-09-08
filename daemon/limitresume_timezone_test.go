package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
)

func TestResumeLimitedSessions_ParsedTimezone(t *testing.T) {
	for _, tc := range []struct{ zone, instant string }{
		{"UTC", "2026-09-07T17:00:00Z"},
		{"Japan", "2026-09-08T08:00:00Z"},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			loc, err := time.LoadLocation("America/Los_Angeles")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 7, 8, 0, 0, 0, loc)
			oldNow := nowFunc
			nowFunc = func() time.Time { return now }
			t.Cleanup(func() { nowFunc = oldNow })
			hit, reset, parsed := task.NewLimitDetector(nil).Check("Claude usage limit reached. Your limit will reset at 5pm ("+tc.zone+")", "claude", now)
			if !hit || !parsed {
				t.Fatal("banner not parsed")
			}
			manager, _, inst, backend := newAutoResumeManager(t, "", true, "finish work", reset)
			want, err := time.Parse(time.RFC3339, tc.instant)
			if err != nil {
				t.Fatal(err)
			}
			now = want.Add(limitResumeGrace - time.Second)
			manager.ResumeLimitedSessions()
			if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
				t.Fatalf("early resume: %v", prompts)
			}
			now = now.Add(time.Second)
			manager.ResumeLimitedSessions()
			if _, _, prompts := backend.snapshot(); len(prompts) != 1 || prompts[0] != "finish work" {
				t.Fatalf("at parsed-zone deadline: prompts=%v", prompts)
			}
			if inst.LimitReached() {
				t.Fatal("still limited at parsed-zone deadline")
			}
		})
	}
}
