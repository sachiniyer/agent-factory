package daemon

import (
	"strings"

	"github.com/sachiniyer/agent-factory/internal/namegen"
)

// The suggested-session-name RPC (#2470). The web New Session modal shows a
// random "adjective-noun" name as shadow text in the title field, and creating
// with the field left empty adopts that name. The web cannot generate the name
// itself: the wordlist is Go-only by the #1970 ruling ("serve the list from the
// daemon; do not duplicate it in the client"), which parity/enum_coverage_test.go
// enforces. So the daemon owns the wordlist (internal/namegen) and serves one
// collision-avoiding name here; the web displays it and echoes it back as
// title_base on an empty submit. The TUI needs no RPC — it calls namegen in
// process — but shares the exact same wordlist, so the two surfaces cannot drift.
//
// It is read-only: it provisions nothing and reserves nothing. The name it
// returns is only a SUGGESTION — the authoritative per-repo uniqueness still runs
// at create time (the title_base auto-suffix walk in manager_create.go), so this
// avoids live titles as a courtesy (a placeholder that already names a session
// reads as a mistake) without needing to be a reservation.

// SuggestSessionNameRequest has no fields: the suggestion avoids every live
// session title the daemon knows, across all repos, so it is collision-free
// whichever project the modal is pointed at. A no-argument POST (jsonFields
// returns nil for it, like ListTasks).
type SuggestSessionNameRequest struct{}

// SuggestSessionNameResponse carries the suggested name.
type SuggestSessionNameResponse struct {
	// Name is a readable "adjective-noun" title (e.g. "brave-otter"), avoiding
	// every live session's title. Always set (the wordlist is non-empty).
	Name string `json:"name"`
}

// suggestName is the wordlist generator the RPC calls. It is a package var (not a
// direct namegen.Suggest call) purely so a test can capture the collision predicate
// the handler builds from live titles and assert it deterministically — the random
// generator cannot be forced into an observable collision otherwise.
var suggestName = namegen.Suggest

// SuggestSessionName returns a random, readable session name that avoids every
// live session's title. It draws from the daemon-owned wordlist so every client
// renders names from the same list.
//
// It deliberately does NOT gate on requireManagerReady: unlike Snapshot as a
// state projection, a name suggestion is best-effort — a placeholder — so serving
// one during the restore window (avoiding whatever titles are already back) is
// strictly better than failing the modal's fetch, and the authoritative per-repo
// uniqueness still runs at create time. Snapshot takes m.mu, so the read is safe
// even mid-restore.
func (s *controlServer) SuggestSessionName(req SuggestSessionNameRequest, resp *SuggestSessionNameResponse) error {
	// Avoid every live title across all repos (empty repo_id = all): titles are
	// per-repo unique, so avoiding them everywhere makes the suggestion safe for
	// whichever project the user ends up picking in the modal.
	taken := make(map[string]bool)
	if s.manager != nil {
		for _, d := range s.manager.Snapshot("") {
			if t := strings.ToLower(strings.TrimSpace(d.Title)); t != "" {
				taken[t] = true
			}
		}
	}
	resp.Name = suggestName(func(name string) bool {
		return taken[strings.ToLower(name)]
	})
	return nil
}
