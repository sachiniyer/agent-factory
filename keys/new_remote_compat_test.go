package keys

import (
	"strings"
	"testing"
)

func TestNewRemoteCompatibilityValidation(t *testing.T) {
	if err := ValidateOverrides(map[string][]string{"new_remote": {"alt+n"}}); err != nil {
		t.Fatal(err)
	}
	err := ValidateOverrides(map[string][]string{"new_remote": {"esc"}})
	if err == nil || !strings.Contains(err.Error(), "opens the creation form with the backend field focused") {
		t.Fatalf("validation must explain the compatibility action: %v", err)
	}
}
