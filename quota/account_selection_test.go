package quota

import (
	"reflect"
	"testing"
)

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
