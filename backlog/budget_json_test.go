package backlog

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/planusage"
)

func budgetUsageFixture() planusage.Usage {
	reset := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	five := planusage.PlanWindow{Plan: 40, Backlog: 10, All: 8, ResetsAt: &reset}
	return planusage.Usage{
		Account:   "default",
		Windows:   planusage.Windows{FiveHour: five},
		ReadingAt: &at,
		Accounts: map[string]planusage.AccountUsage{"default": {
			Windows: planusage.Windows{FiveHour: five}, ReadingAt: &at, Unpriced: []string{"claude-x-9"},
		}},
		Runs:        map[string]float64{"s1": 3.5},
		RunWindow:   map[string]float64{"s1": 2},
		Unmeterable: map[string]string{"s2": "the runner can't meter aider"},
	}
}

func TestBudgetResponseJSONShape(t *testing.T) {
	out, err := json.Marshal(BudgetResponse{Budget: *NewBudget(), Usage: budgetUsageFixture()})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"budget":{"schema_version":2,"limit":50,"max_per_run":10,"usd_limit":0,"usd_spent":0},` +
		`"account":"default",` +
		`"windows":{"five_hour":{"plan":40,"backlog":10,"all_cost":8,"resets_at":"2026-10-20T14:00:00Z"},"seven_day":{"plan":0,"backlog":0,"all_cost":0,"resets_at":null}},` +
		`"reading_at":"2026-10-20T12:00:00Z",` +
		`"accounts":{"default":{"windows":{"five_hour":{"plan":40,"backlog":10,"all_cost":8,"resets_at":"2026-10-20T14:00:00Z"},"seven_day":{"plan":0,"backlog":0,"all_cost":0,"resets_at":null}},"reading_at":"2026-10-20T12:00:00Z","unpriced":["claude-x-9"]}},` +
		`"runs":{"s1":3.5},"run_window":{"s1":2},"unmeterable":{"s2":"the runner can't meter aider"}}`
	if string(out) != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
}

func TestSpendEventJSONShape(t *testing.T) {
	out, err := json.Marshal(budgetUsageFixture().SpendEvent())
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"five_hour":{"plan":40,"backlog":10,"all_cost":8,"resets_at":"2026-10-20T14:00:00Z"},` +
		`"seven_day":{"plan":0,"backlog":0,"all_cost":0,"resets_at":null},` +
		`"reading_at":"2026-10-20T12:00:00Z",` +
		`"runs":{"s1":3.5},"run_window":{"s1":2},"unmeterable":{"s2":"the runner can't meter aider"}}`
	if string(out) != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
	empty, _ := json.Marshal(planusage.Usage{}.SpendEvent())
	if string(empty) != `{"five_hour":{"plan":0,"backlog":0,"all_cost":0,"resets_at":null},"seven_day":{"plan":0,"backlog":0,"all_cost":0,"resets_at":null},"reading_at":null,"runs":{},"run_window":{},"unmeterable":{}}` {
		t.Fatalf("empty maps must be {}: %s", empty)
	}
}
