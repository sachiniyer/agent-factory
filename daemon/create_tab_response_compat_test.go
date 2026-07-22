package daemon

import (
	"bytes"
	"encoding/gob"
	"testing"
)

type createTabResponseBeforeStableID struct {
	Name     string
	TmuxName string
}

func TestCreateTabResponseGobMixedVersionCompatibility(t *testing.T) {
	t.Run("new client decodes old daemon response", func(t *testing.T) {
		var wire bytes.Buffer
		if err := gob.NewEncoder(&wire).Encode(createTabResponseBeforeStableID{
			Name: "shell", TmuxName: "af_alpha__shell",
		}); err != nil {
			t.Fatalf("encode old response: %v", err)
		}
		var got CreateTabResponse
		if err := gob.NewDecoder(&wire).Decode(&got); err != nil {
			t.Fatalf("decode old response into new shape: %v", err)
		}
		if got.ID != "" || got.Name != "shell" || got.TmuxName != "af_alpha__shell" {
			t.Fatalf("decoded old response = %+v; want explicit empty-id compatibility", got)
		}
	})

	t.Run("old client ignores new daemon id", func(t *testing.T) {
		var wire bytes.Buffer
		if err := gob.NewEncoder(&wire).Encode(CreateTabResponse{
			ID: "daemon-tab-id", Name: "shell", TmuxName: "af_alpha__shell",
		}); err != nil {
			t.Fatalf("encode new response: %v", err)
		}
		var got createTabResponseBeforeStableID
		if err := gob.NewDecoder(&wire).Decode(&got); err != nil {
			t.Fatalf("decode new response into old shape: %v", err)
		}
		if got.Name != "shell" || got.TmuxName != "af_alpha__shell" {
			t.Fatalf("decoded new response in old shape = %+v", got)
		}
	})
}
