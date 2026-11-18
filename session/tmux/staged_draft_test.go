package tmux

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// The recheck verdicts behind the daemon's staged-draft watch (#7545), judged
// on the same physical-grid pane shapes the #4200 remedy reads. The fixtures
// model the pane at RECHECK time — minutes or days after the send — so the
// transcript echo of a submitted prompt, a user's own typed draft, and the
// still-staged bound draft each get their own verdict.

const stagedTailText = "the end of the staged prompt"

func stagedTailEvidence() stagedDraftEvidence {
	return stagedDraftEvidence{tail: normalizeDelivery(stagedTailText)}
}

// claudeSubmittedPane is a delivered send's pane: the prompt's transcript
// echo sits ABOVE a bare live composer — the shape every sent-unverified
// report resolves to once the turn visibly begins.
func claudeSubmittedPane() string {
	return strings.Join([]string{
		"❯ " + stagedTailText,
		"✻ Cogitated for 1m 2s · done 3:53 AM",
		"❯",
		"",
		"  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n")
}

func TestStagedDraftReading(t *testing.T) {
	tests := []struct {
		name string
		pane string
		ev   stagedDraftEvidence
		want StagedDraftRead
	}{
		{
			// The #7545 shape: a sent-unverified send's bound tail still
			// sits in the idle composer's live draft.
			name: "bound tail staged in the live composer",
			pane: claudeInsertPane("❯ " + stagedTailText),
			ev:   stagedTailEvidence(),
			want: StagedDraftStaged,
		},
		{
			name: "bound chip staged in the live composer",
			pane: claudeInsertPane("❯ " + claudeChip),
			ev:   stagedDraftEvidence{chip: normalizeDelivery(claudeChip)},
			want: StagedDraftStaged,
		},
		{
			// The common delivered-but-unverified resolution: the bound
			// evidence is a transcript echo above the composer, so the
			// watch ends — the Captain's named case.
			name: "bound tail echoed above the composer is submitted",
			pane: claudeSubmittedPane(),
			ev:   stagedTailEvidence(),
			want: StagedDraftSubmitted,
		},
		{
			name: "bound chip echoed above the composer is submitted",
			pane: strings.Join([]string{
				"❯ " + claudeChip,
				"✻ Cogitated for 1m 2s · done 3:53 AM",
				"❯",
			}, "\n"),
			ev:   stagedDraftEvidence{chip: normalizeDelivery(claudeChip)},
			want: StagedDraftSubmitted,
		},
		{
			name: "evidence nowhere in the frame is gone",
			pane: claudeInsertPane("❯"),
			ev:   stagedTailEvidence(),
			want: StagedDraftGone,
		},
		{
			// A user's own typed draft is never flagged as af's: it
			// carries none of the bound evidence, so the send's watch
			// ends rather than matching arbitrary composer contents.
			name: "a user's own typed draft is not the send's",
			pane: claudeInsertPane("❯ their own instructions, typed by hand"),
			ev:   stagedTailEvidence(),
			want: StagedDraftGone,
		},
		{
			// Echo above AND draft staged — the send submitted one copy
			// and another still sits in the composer: still staged wins.
			name: "staged beats submitted when both reads are present",
			pane: strings.Join([]string{
				"❯ " + stagedTailText,
				"✻ done",
				"❯ " + stagedTailText,
			}, "\n"),
			ev:   stagedTailEvidence(),
			want: StagedDraftStaged,
		},
		{
			// A frame with no composer-glyph row at all — a dialog or a
			// non-composer screen — cannot be judged. Blind is never a
			// negative, so the watch neither resets nor resolves.
			name: "no composer glyph is unreadable",
			pane: "✻ Working…\n  esc to interrupt",
			ev:   stagedTailEvidence(),
			want: StagedDraftUnreadable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, why := stagedDraftReading(tc.pane, tc.ev)
			if got != tc.want {
				t.Fatalf("stagedDraftReading = %v (%s), want %v", got, why, tc.want)
			}
			if why == "" {
				t.Fatal("every verdict names its reason for the daemon log")
			}
		})
	}
}

// The TmuxSession method's own branches: no stashed evidence costs no
// capture, and a capture that cannot run is Unreadable — never a negative
// about the draft.
func TestBoundDraftStillStagedWithoutAPane(t *testing.T) {
	s := &TmuxSession{}
	if read, _ := s.BoundDraftStillStaged(); read != StagedDraftNoEvidence {
		t.Fatalf("no stashed evidence: read %v, want StagedDraftNoEvidence", read)
	}
	s.stagedDraft.Store(&stagedDraftEvidence{tail: "bound"})
	s.cmdExec = cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return errors.New("tmux is gone") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, errors.New("tmux is gone") },
	}
	if read, _ := s.BoundDraftStillStaged(); read != StagedDraftUnreadable {
		t.Fatalf("a failed capture: read %v, want StagedDraftUnreadable", read)
	}
}
