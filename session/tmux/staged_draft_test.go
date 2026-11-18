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
			// Codex's Ultra tier draws » at the composer, which
			// codexComposerGlyphs already knows — a glyph the classifier
			// missed read Unreadable forever and retired the watch blind
			// (Codex #7547, third round).
			name: "bound chip staged in an Ultra codex composer",
			pane: codexWarningPane("» "+codexChip, false),
			ev:   stagedDraftEvidence{chip: normalizeDelivery(codexChip)},
			want: StagedDraftStaged,
		},
		{
			// A footer glued to the composer with no blank row between is
			// composer content as far as the region can tell, so the
			// literal tail comparisons are unprovable — master's own
			// ruling withholds there too (stillChipComposer). Unreadable,
			// never Gone: Gone would close a live watch on a genuinely
			// staged draft (Codex #7547).
			name: "whole tail with a footer glued to the composer",
			pane: codexWarningPane("› continue", false),
			ev:   stagedDraftEvidence{tail: "continue", tailWhole: true},
			want: StagedDraftUnreadable,
		},
		{
			// The same shape with a blank row before the footer bounds
			// the region — the literal tail may compare.
			name: "whole tail staged above a blank-gap footer",
			pane: codexWarningPane("› continue", true),
			ev:   stagedDraftEvidence{tail: "continue", tailWhole: true},
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
			// The staged check searches the composer region, never every
			// row below the glyph: a bound tail that appears only in the
			// status/footer line under the composer is not the draft
			// (Codex #7547 — a short "on" send must not alias the footer's
			// "bypass permissions on").
			name: "a bound tail matching only the footer is gone",
			pane: claudeInsertPane("❯"),
			ev:   stagedDraftEvidence{tail: normalizeDelivery("bypass permissions")},
			want: StagedDraftGone,
		},
		{
			// composerRegion stops at the first content row after an
			// interior blank, so a multiline draft's tail read Gone and
			// closed the watch for good (Codex #7547). The frame's own
			// border proves the stranded row interior — the tail still
			// reads staged.
			name: "a bound tail below an interior blank is still staged",
			pane: strings.Join([]string{
				"✻ Cogitated for 4m 47s · done 3:53 AM",
				"❯ first paragraph",
				"",
				"  final instruction",
				"────────────────────────────────────────────────────────────────────────────────",
				"",
				"  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)",
			}, "\n"),
			ev:   stagedDraftEvidence{tail: normalizeDelivery("firstparagraphfinalinstruction")},
			want: StagedDraftStaged,
		},
		{
			// The same stranded-content shape WITHOUT a border closing
			// the frame below it is the footer, not the draft — the
			// border judgment is what keeps the interior-blank fix from
			// reopening the footer leak.
			name: "content past an unbordered blank is still the footer",
			pane: strings.Join([]string{
				"✻ Cogitated for 4m 47s · done 3:53 AM",
				"❯",
				"",
				"  " + stagedTailText,
			}, "\n"),
			ev:   stagedTailEvidence(),
			want: StagedDraftGone,
		},
		{
			// A `>`-leading Markdown quote inside a multiline draft
			// normalizes to a composer glyph row, so the anchor can sit
			// inside the draft with earlier lines above it — the tail is
			// split across the anchor and neither side holds it. Gone
			// would permanently close a live send on a guess; blind is
			// honest (Codex #7547, second round).
			name: "a quote-ambiguous anchor is unreadable, not gone",
			pane: strings.Join([]string{
				"✻ Cogitated for 4m 47s · done 3:53 AM",
				"❯ first paragraph",
				"  > final instruction",
				"────────────────────────────────────────────────────────────────────────────────",
				"",
				"  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)",
			}, "\n"),
			ev:   stagedDraftEvidence{tail: normalizeDelivery("firstparagraph>finalinstruction")},
			want: StagedDraftUnreadable,
		},
		{
			// The same ambiguous shape is still honest Staged when the
			// bound tail sits entirely below the `>` row — under both
			// readings that text is inside the live draft, so the
			// ambiguity must not blanket-downgrade the flag.
			name: "a tail below a quote-ambiguous anchor is still staged",
			pane: strings.Join([]string{
				"✻ Cogitated for 4m 47s · done 3:53 AM",
				"❯ first paragraph",
				"  > " + stagedTailText,
				"────────────────────────────────────────────────────────────────────────────────",
			}, "\n"),
			ev:   stagedTailEvidence(),
			want: StagedDraftStaged,
		},
		{
			// An unboxed composer can leave a submitted echo immediately
			// above the fresh glyph row with no border or output between
			// — the same shape as a quote continuation. Submitted would
			// close the watch on a guess, so blind is honest here too.
			name: "a glyph echo adjacent to the composer is unreadable",
			pane: strings.Join([]string{
				"› " + stagedTailText,
				"›",
			}, "\n"),
			ev:   stagedTailEvidence(),
			want: StagedDraftUnreadable,
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
			// Echo above AND matching draft in the composer — the send
			// provably landed (a NEW echo since the boundary), so the
			// composer copy is the user's recall or retype, never af's to
			// flag (Codex #7547 independent review: submitted demotes the
			// composer match).
			name: "a bound tail that also echoes above the anchor is submitted",
			pane: strings.Join([]string{
				"❯ " + stagedTailText,
				"✻ done",
				"❯ " + stagedTailText,
			}, "\n"),
			ev:   stagedTailEvidence(),
			want: StagedDraftSubmitted,
		},
		{
			// The review's interleaving: af's send submitted but recorded
			// sent-unverified, the turn ran, then the user retyped the
			// prompt with an edit. The composer copy is theirs — a
			// whole-payload tail must EQUAL the text after the glyph, so
			// the edit breaks the match outright.
			name: "a user retyping the sent text with an edit is gone",
			pane: claudeInsertPane("❯ continue, but skip docs"),
			ev:   stagedDraftEvidence{tail: "continue", tailWhole: true},
			want: StagedDraftGone,
		},
		{
			// And the recall twin: the user up-arrowed the submitted
			// prompt back into the composer verbatim. The new echo above
			// the anchor proves the send landed; the matching composer
			// text is unattributable, never af's still-staged draft.
			name: "a user's up-arrow recall of the submitted prompt is submitted",
			pane: strings.Join([]string{
				"❯ continue",
				"✻ done",
				"❯ continue",
			}, "\n"),
			ev:   stagedDraftEvidence{tail: "continue", tailWhole: true},
			want: StagedDraftSubmitted,
		},
		{
			// The same recall with the echo scrolled out of view: the
			// composer copy is verbatim, nothing above the anchor
			// matches, and the boundary's transcript tail is gone from
			// the bounded capture — so the count's silence cannot be
			// trusted. Never Staged, never Gone (Codex #7547, fourth
			// round): the blind budget retires the watch.
			name: "a user's up-arrow recall whose echo scrolled off is unreadable",
			pane: strings.Join([]string{
				"✻ done",
				"❯ continue",
			}, "\n"),
			ev:   stagedDraftEvidence{tail: "continue", tailWhole: true, boundaryMark: "boundarytranscripttail"},
			want: StagedDraftUnreadable,
		},
		{
			// The chip twin: a bound paste chip recalled into the
			// composer while the boundary's line is out of reach — the
			// chip is still af's own text, but the echo may have scrolled,
			// so it is blind, never a flag.
			name: "a user's recalled chip whose boundary scrolled off is unreadable",
			pane: strings.Join([]string{
				"✻ done",
				"❯ " + claudeChip,
			}, "\n"),
			ev:   stagedDraftEvidence{chip: normalizeDelivery(claudeChip), boundaryMark: "boundarytranscripttail"},
			want: StagedDraftUnreadable,
		},
		{
			// The boundary's line still in view proves nothing scrolled
			// since the send — a genuinely staged tail may flag.
			name: "a staged tail with the boundary still in view is staged",
			pane: claudeInsertPane("❯ continue"),
			ev:   stagedDraftEvidence{tail: "continue", tailWhole: true, boundaryMark: "Cogitatedfor4m47s"},
			want: StagedDraftStaged,
		},
		{
			// A grown echo resolves Submitted even from deep in the
			// scrollback — the count rose past the boundary's baseline,
			// which needs no completeness proof.
			name: "a grown echo deep in the transcript is submitted",
			pane: strings.Join([]string{
				"the boundary's transcript tail",
				"❯ continue",
				"✻ done",
				"❯ continue",
			}, "\n"),
			ev:   stagedDraftEvidence{tail: "continue", tailWhole: true, boundaryMark: "boundarytranscripttail"},
			want: StagedDraftSubmitted,
		},
		{
			// A payload of 32 runes or fewer binds its whole text as the
			// tail (submit.go: it IS the completion). Contained is not
			// enough — "yes" sent must not read staged inside a user's
			// "they established".
			name: "a short payload matches only as the whole composer text",
			pane: claudeInsertPane("❯ they established"),
			ev:   stagedDraftEvidence{tail: "yes", tailWhole: true},
			want: StagedDraftGone,
		},
		{
			// An identical echo that predates the send — the same prompt
			// went in before (#1146's limit-resume resend) — leaves the
			// composer copy attributable to the user's recall of THAT
			// echo. Unprovable either way: blind, never a negative and
			// never a false flag.
			name: "a pre-existing identical echo makes the composer match unreadable",
			pane: strings.Join([]string{
				"❯ " + stagedTailText,
				"✻ done",
				"❯ " + stagedTailText,
			}, "\n"),
			ev:   stagedDraftEvidence{tail: normalizeDelivery(stagedTailText), tailEcho: 1},
			want: StagedDraftUnreadable,
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
