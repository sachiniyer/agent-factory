package sessionenv

import "testing"

func TestStripDeclaredArgs(t *testing.T) {
	flag := []string{"-c", "check_for_update_on_startup=false"}
	cases := []struct {
		name     string
		program  string
		declared []string
		want     string
		ok       bool
	}{
		{"trailing block", "codex -c check_for_update_on_startup=false", flag, "codex", true},
		{"middle block keeps the user's words", "codex --model x -c check_for_update_on_startup=false --verbose", flag, "codex --model x --verbose", true},
		{"mid-command resume splice", "codex resume abc -c check_for_update_on_startup=false", []string{"resume", "abc", "-c", "check_for_update_on_startup=false"}, "codex", true},
		{"empty declaration is unchanged", "codex --model x", nil, "codex --model x", true},
		{"declaration not present", "codex --model x", flag, "", false},
		{"split declaration is not a contiguous block", "codex -c a=b --x -c check_for_update_on_startup=false", []string{"-c", "a=b", "-c", "check_for_update_on_startup=false"}, "", false},
		{"never strips the executable", "codex", []string{"codex"}, "", false},
		{"remaining word needing quotes is refused", "codex 'a b' -c check_for_update_on_startup=false", flag, "", false},
		{"not a literal simple command", "codex $X -c check_for_update_on_startup=false", flag, "", false},
		{"not a single command", "codex -c k=v; rm -rf x", []string{"-c", "k=v"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := StripDeclaredArgs(tc.program, tc.declared)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("StripDeclaredArgs(%q, %v) = (%q, %v), want (%q, %v)", tc.program, tc.declared, got, ok, tc.want, tc.ok)
			}
		})
	}
}
