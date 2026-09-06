package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestComparePrompt(t *testing.T) {
	for _, tc := range []struct {
		name, live                     string
		missing, unreadable, wantError bool
	}{
		{name: "identical", live: "watch\n"},
		{name: "whitespace", live: "watch", wantError: true},
		{name: "different", live: "changed\n", wantError: true},
		{name: "missing file", missing: true, wantError: true},
		{name: "unreadable task", unreadable: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "prompt.md")
			if !tc.missing {
				if err := os.WriteFile(file, []byte("watch\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := comparePrompt("test", file, func() ([]byte, error) {
				if tc.unreadable {
					return nil, errors.New("unavailable")
				}
				return json.Marshal(map[string]any{"data": map[string]string{"id": "test", "prompt": tc.live}, "error": nil})
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}

func TestComparePromptInvalidTask(t *testing.T) {
	file := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(file, []byte("watch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`invalid`, `{"error":{"message":"unavailable"}}`, `{"data":null}`, `{"data":{"id":"other","prompt":"watch\n"}}`, `{"data":{"id":"test"}}`} {
		if err := comparePrompt("test", file, func() ([]byte, error) { return []byte(raw), nil }); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
