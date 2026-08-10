package quota

import (
	"reflect"
	"testing"
)

func TestSelectAccountCandidate_LimitedAmbientSessionUsesConfiguredAccount(t *testing.T) {
	got, ok := SelectAccountCandidate(AccountSelection{
		Candidates: []string{"work", "personal"},
		Registered: []string{"personal", "work"},
	})
	if !ok || got != "work" {
		t.Fatalf("SelectAccountCandidate = (%q, %v), want (work, true)", got, ok)
	}
}

func TestSelectAccountCandidate_AllCandidatesLimitedFallsBackToWait(t *testing.T) {
	got, ok := SelectAccountCandidate(AccountSelection{
		Candidates: []string{"work", "personal"},
		Registered: []string{"personal", "work"},
		Limited:    []string{"work", "personal"},
	})
	if ok || got != "" {
		t.Fatalf("SelectAccountCandidate = (%q, %v), want no swap", got, ok)
	}
}

func TestSelectAccountCandidate_ExplicitAccountPinIsNeverOverridden(t *testing.T) {
	got, ok := SelectAccountCandidate(AccountSelection{
		CurrentAccount: "work",
		Candidates:     []string{"personal"},
		Registered:     []string{"personal", "work"},
	})
	if ok || got != "" {
		t.Fatalf("SelectAccountCandidate = (%q, %v), want explicit account pin preserved", got, ok)
	}
}

func TestSelectAccountCandidates_PriorAutomaticSelectionThenConfiguredOrder(t *testing.T) {
	got := SelectAccountCandidates(AccountSelection{
		CurrentAccount:      "personal",
		CurrentAutoSelected: true,
		Candidates:          []string{"work", "personal", "work", "backup"},
		Registered:          []string{"personal", "work", "backup"},
		Limited:             []string{"backup"},
	})
	want := []string{"personal", "work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectAccountCandidates = %q, want %q", got, want)
	}
}
