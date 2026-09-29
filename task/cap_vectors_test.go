package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// capVectors drives the shared vector file testdata/cap_vectors.json — the
// contract the web implementation (web/src/tasks.ts capUnavailableReason /
// parseCapInput) is pinned against by web/src/tasks_cap.test.ts reading the
// same file. A change here must change the vectors and the twin together, not
// one side alone (#4180: the two UIs disagreeing about what is valid is the
// bug this exists to prevent).
type capVectors struct {
	Applies []struct {
		Name    string `json:"name"`
		IsWatch bool   `json:"is_watch"`
		Target  string `json:"target"`
		Applies bool   `json:"applies"`
		Reason  string `json:"reason"`
	} `json:"applies"`
	Parse []struct {
		Name  string `json:"name"`
		Raw   string `json:"raw"`
		OK    bool   `json:"ok"`
		Value int64  `json:"value"`
	} `json:"parse"`
}

func loadCapVectors(t *testing.T) capVectors {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "cap_vectors.json"))
	if err != nil {
		t.Fatalf("read cap vectors: %v", err)
	}
	var vf capVectors
	if err := json.Unmarshal(data, &vf); err != nil {
		t.Fatalf("parse cap vectors: %v", err)
	}
	if len(vf.Applies) == 0 || len(vf.Parse) == 0 {
		t.Fatal("cap vectors file must carry both applies and parse vectors")
	}
	return vf
}

// TestCapAppliesVectors pins CapApplies and CapUnavailableReason to the shared
// file — including the check ORDER, which decides which reason a shape failing
// both rules reports.
func TestCapAppliesVectors(t *testing.T) {
	for _, v := range loadCapVectors(t).Applies {
		t.Run(v.Name, func(t *testing.T) {
			if got := CapApplies(v.IsWatch, v.Target); got != v.Applies {
				t.Errorf("CapApplies(watch=%v, %q) = %v, want %v", v.IsWatch, v.Target, got, v.Applies)
			}
			if got := CapUnavailableReason(v.IsWatch, v.Target); got != v.Reason {
				t.Errorf("CapUnavailableReason(watch=%v, %q) = %q, want %q", v.IsWatch, v.Target, got, v.Reason)
			}
			// The record-shaped entry point must agree with the shape-shaped
			// one — a watch task is a non-empty watch_cmd, so build the record
			// the vectors describe and ask it the same question.
			rec := Task{TargetSession: v.Target}
			if v.IsWatch {
				rec.WatchCmd = "watch-src"
			} else {
				rec.CronExpr = "* * * * *"
			}
			if got := rec.capApplies(); got != v.Applies {
				t.Errorf("Task{watch=%v, target=%q}.capApplies() = %v, want %v", v.IsWatch, v.Target, got, v.Applies)
			}
		})
	}
}

// TestCapParseVectors pins ParseCapInput's accept/refuse decisions — and the
// safe-integer ceiling — to the file the web twin reads.
func TestCapParseVectors(t *testing.T) {
	for _, v := range loadCapVectors(t).Parse {
		t.Run(v.Name, func(t *testing.T) {
			got, ok := ParseCapInput(v.Raw)
			if ok != v.OK {
				t.Fatalf("ParseCapInput(%q) ok = %v, want %v", v.Raw, ok, v.OK)
			}
			if ok && int64(got) != v.Value {
				t.Errorf("ParseCapInput(%q) = %d, want %d", v.Raw, got, v.Value)
			}
		})
	}
}
