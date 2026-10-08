package quota

import (
	"reflect"
	"testing"
)

func TestSelectAccountCandidates_LimitedAmbientSessionUsesConfiguredAccount(t *testing.T) {
	got := SelectAccountCandidates(AccountSelection{
		Candidates: []string{"work", "personal"},
		Registered: []string{"personal", "work"},
	})
	want := []string{"work", "personal"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectAccountCandidates = %q, want %q", got, want)
	}
}

func TestSelectAccountCandidates_AllCandidatesLimitedFallsBackToWait(t *testing.T) {
	got := SelectAccountCandidates(AccountSelection{
		Candidates: []string{"work", "personal"},
		Registered: []string{"personal", "work"},
		Limited:    []string{"work", "personal"},
	})
	if len(got) != 0 {
		t.Fatalf("SelectAccountCandidates = %q, want no swap", got)
	}
}

func TestSelectAccountCandidates_ExplicitAccountPinIsNeverOverridden(t *testing.T) {
	got := SelectAccountCandidates(AccountSelection{
		CurrentAccount: "work",
		Candidates:     []string{"personal"},
		Registered:     []string{"personal", "work"},
	})
	if len(got) != 0 {
		t.Fatalf("SelectAccountCandidates = %q, want explicit account pin preserved", got)
	}
}

func TestSelectAccountCandidates_CurrentAutomaticAccountIsNotAReplacement(t *testing.T) {
	got := SelectAccountCandidates(AccountSelection{
		CurrentAccount:      "work",
		CurrentAutoSelected: true,
		Candidates:          []string{"work"},
		Registered:          []string{"work"},
	})
	if len(got) != 0 {
		t.Fatalf("SelectAccountCandidates = %q, want ordinary same-account resume", got)
	}
}

func TestSelectAccountCandidates_SkipsCurrentAutomaticSelectionAndKeepsConfiguredOrder(t *testing.T) {
	got := SelectAccountCandidates(AccountSelection{
		CurrentAccount:      "personal",
		CurrentAutoSelected: true,
		Candidates:          []string{"work", "personal", "work", "backup"},
		Registered:          []string{"personal", "work", "backup"},
		Limited:             []string{"backup"},
	})
	want := []string{"work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectAccountCandidates = %q, want %q", got, want)
	}
}
