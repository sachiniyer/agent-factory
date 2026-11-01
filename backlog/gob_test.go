package backlog

import (
	"bytes"
	"encoding/gob"
	"reflect"
	"testing"
)

func gobRoundTrip[T any](t *testing.T, in T) T {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(in); err != nil {
		t.Fatal(err)
	}
	var out T
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGobKeepsZeroPointers(t *testing.T) {
	u := UpdateItemRequest{RepoID: repoID, ID: 2, Body: ptr(""), Program: ptr(""), WaitsOn: ptr([]int{}), Sequential: ptr(false)}
	got := gobRoundTrip(t, u)
	if got.Body == nil || got.Program == nil || got.WaitsOn == nil || got.Sequential == nil || got.Title != nil || got.ID != 2 {
		t.Fatalf("%+v", got)
	}
	s := SetSettingsRequest{RepoID: repoID, Runner: ptr(false)}
	if got := gobRoundTrip(t, s); got.Runner == nil || *got.Runner || got.MaxRunning != nil {
		t.Fatalf("%+v", got)
	}
	b := SetBudgetRequest{Limit: ptr(0.0), USDLimit: ptr(0.0)}
	if got := gobRoundTrip(t, b); !reflect.DeepEqual(got, b) {
		t.Fatalf("%+v", got)
	}
}
