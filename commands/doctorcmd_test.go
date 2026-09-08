package commands

import (
	"testing"

	"github.com/sachiniyer/agent-factory/doctor"
)

func TestDoctorExitCode(t *testing.T) {
	for _, tt := range []struct {
		name       string
		unresolved bool
		incomplete []string
		want       int
	}{
		{name: "clean", want: 0},
		{name: "incomplete only", incomplete: []string{"stale-temp-home"}, want: 1},
		{name: "unresolved only", unresolved: true, want: 1},
		{name: "unresolved and incomplete", unresolved: true, incomplete: []string{"stale-temp-home"}, want: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := &doctor.Report{Incomplete: tt.incomplete}
			if tt.unresolved {
				report.Findings = []doctor.Finding{{Actionable: true}}
			}
			if got := doctorExitCode(report); got != tt.want {
				t.Errorf("doctorExitCode(unresolved=%d, incomplete=%v) = %d, want %d", report.UnresolvedCount(), report.Incomplete, got, tt.want)
			}
		})
	}
}
