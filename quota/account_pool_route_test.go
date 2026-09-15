package quota

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// RouteAccountPool is the create-time half of the account pool (#4404): a new
// session with no --account launches under a registered account that carries no
// current usage-limit evidence, preferring the configured default and spreading
// load across the rest. These tests pin that contract at the pure-function
// boundary; the daemon wires the registry and evidence in.

func TestRouteAccountPoolSkipsWalledAccounts(t *testing.T) {
	got, err := RouteAccountPool(
		"codex",
		[]string{"codex1", "codex2"},
		map[string]time.Time{"codex1": time.Now().Add(time.Hour)},
		"",
		nil,
	)
	if err != nil || got != "codex2" {
		t.Fatalf("RouteAccountPool = (%q, %v), want (codex2, nil)", got, err)
	}
}

func TestRouteAccountPoolPrefersTheConfiguredDefault(t *testing.T) {
	got, err := RouteAccountPool(
		"codex",
		[]string{"codex1", "codex2"},
		nil,
		"codex2",
		map[string]int{"codex1": 0, "codex2": 3},
	)
	if err != nil || got != "codex2" {
		t.Fatalf("RouteAccountPool = (%q, %v), want (codex2, nil): a healthy default stays the pick even when it is busier", got, err)
	}
}

func TestRouteAccountPoolSkipsAWalledDefault(t *testing.T) {
	got, err := RouteAccountPool(
		"codex",
		[]string{"codex1", "codex2"},
		map[string]time.Time{"codex1": time.Now().Add(time.Hour)},
		"codex1",
		nil,
	)
	if err != nil || got != "codex2" {
		t.Fatalf("RouteAccountPool = (%q, %v), want (codex2, nil): a walled default is a preference, not a pin", got, err)
	}
}

func TestRouteAccountPoolSpreadsToTheLeastLoadedHealthyAccount(t *testing.T) {
	got, err := RouteAccountPool(
		"codex",
		[]string{"codex1", "codex2", "codex3"},
		map[string]time.Time{"codex3": time.Now().Add(time.Hour)},
		"",
		map[string]int{"codex1": 2, "codex2": 0},
	)
	if err != nil || got != "codex2" {
		t.Fatalf("RouteAccountPool = (%q, %v), want (codex2, nil)", got, err)
	}
}

func TestRouteAccountPoolTieBreaksByRegistrationOrder(t *testing.T) {
	got, err := RouteAccountPool(
		"codex",
		[]string{"codex1", "codex2"},
		nil,
		"",
		map[string]int{"codex1": 1, "codex2": 1},
	)
	if err != nil || got != "codex1" {
		t.Fatalf("RouteAccountPool = (%q, %v), want (codex1, nil)", got, err)
	}
}

func TestRouteAccountPoolIgnoresEvidenceForUnregisteredAccounts(t *testing.T) {
	got, err := RouteAccountPool(
		"codex",
		[]string{"codex1"},
		map[string]time.Time{"deleted-account": time.Now().Add(time.Hour)},
		"",
		nil,
	)
	if err != nil || got != "codex1" {
		t.Fatalf("RouteAccountPool = (%q, %v), want (codex1, nil): evidence about an account that is not registered cannot wall the pool", got, err)
	}
}

func TestRouteAccountPoolAllWalledReportsTheEarliestReset(t *testing.T) {
	earliest := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	later := earliest.Add(48 * time.Hour)
	got, err := RouteAccountPool(
		"codex",
		[]string{"codex1", "codex2"},
		map[string]time.Time{"codex1": later, "codex2": earliest},
		"",
		nil,
	)
	if got != "" || err == nil {
		t.Fatalf("RouteAccountPool = (%q, %v), want an all-walled refusal", got, err)
	}
	var walled *AllAccountsWalledError
	if !errors.As(err, &walled) {
		t.Fatalf("RouteAccountPool error = %T, want *AllAccountsWalledError", err)
	}
	if !walled.Earliest.Equal(earliest) || walled.EarliestOn != "codex2" {
		t.Fatalf("walled = %+v, want earliest reset %s on codex2", walled, earliest)
	}
	if msg := err.Error(); !containsAll(msg, "codex1", "codex2", earliest.UTC().Format(time.RFC3339)) {
		t.Fatalf("all-walled error %q must name the pool and the earliest reset", msg)
	}
}

func TestRouteAccountPoolAllWalledWithNoKnownReset(t *testing.T) {
	_, err := RouteAccountPool(
		"codex",
		[]string{"codex1"},
		map[string]time.Time{"codex1": {}},
		"",
		nil,
	)
	var walled *AllAccountsWalledError
	if !errors.As(err, &walled) {
		t.Fatalf("RouteAccountPool error = %v, want *AllAccountsWalledError", err)
	}
	if !walled.Earliest.IsZero() {
		t.Fatalf("walled.Earliest = %s, want unknown reset reported as zero", walled.Earliest)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
