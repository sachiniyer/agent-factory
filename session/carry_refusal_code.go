package session

import (
	"regexp"
	"strings"

	"github.com/sachiniyer/agent-factory/handoff"
)

// The refusals a carry can give before its copy, named so that each one's
// carry refusal code (handoff.Carry*) is defined beside its prose. The prose
// is what the pane, the API's carry_failure and af's log say. The code is all
// the handoff engine's carry-refused event carries, because the prose can embed
// a path or an account name (#7156 review P3-2).
const (
	carryNoLaunchCommandReason         = "af has no record of the command this session launched, so it cannot locate its conversation"
	carryNothingAppliesReason          = "af found no conversation carry that applies to this session"
	carryUnresumableReason             = "the resolved program cannot resume a specific conversation"
	carryNoRegistryReason              = "af could not locate its account registry"
	carryTargetUnresolvedReason        = "the new account's home could not be resolved"
	carryTargetMisplacedReason         = "the new account's home is not where af registers accounts"
	carrySourceMisplacedReason         = "the previous account's home is not where af registers accounts"
	carryNoAmbientClaudeReason         = "af could not resolve the ambient Claude config directory"
	carryNoAmbientCodexReason          = "af could not resolve the ambient Codex home"
	carryNoClaudeDirReason             = "af could not resolve the directory Claude files this conversation under"
	carryTranscriptMissingReason       = "its transcript is missing from the previous account's home"
	carryRolloutMissingReason          = "its rollout is missing from the previous account's home"
	carryStoreUnlistedReason           = "af could not enumerate the Codex rollout store"
	carrySourceStoreUnlistedReason     = "af could not enumerate the previous account's rollout store"
	carryTranscriptsUnlistedReason     = "af could not list the conversations in this worktree to confirm the recorded one is current"
	carryTranscriptCopiesReason        = "Claude holds a newer copy of this conversation under another spelling of the worktree's path than the one af would carry"
	carryUncorrelatedReason            = "af could not confirm the recorded conversation is still the newest one in this worktree"
	carryRolloutMisplacedReason        = "af could not place the Codex rollout inside its home"
	carryOtherFormsUnlistedReason      = "af could not check whether its rollout is stored compressed or as a thread/revert rollout"
	carryOtherFormsReason              = "its rollout is stored compressed or as a thread/revert rollout, which af does not carry yet"
	carryTargetHoldsOtherRolloutReason = "the new account already files this conversation under a different rollout"
	carryForkUnreadableReason          = "af could not read its rollout's session metadata to confirm the rollout holds the whole conversation"
)

// The refusals that name their subject, as formats.
const (
	carryUnrecordedFormat         = "af had no recorded %s conversation id for the previous session"
	carryForeignIDFormat          = "the recorded conversation id is not a %s session id"
	carrySourceUnregisteredFormat = "the previous account %q is no longer registered for %s"
	carryUnsupportedFormat        = "%s conversations cannot be carried"
	carryDuplicatedFormat         = "%d Codex rollouts record this conversation"
	carrySymlinkFormat            = "%s is a symbolic link, which af does not follow when carrying a conversation"
)

// carryOperatorRepairMark is in every refusal of an operator-recorded
// conversation whose provenance af could not confirm (operatorRepairProvenance).
const carryOperatorRepairMark = "operator-recorded conversation"

var carryRefusalCodeOf = map[string]string{
	carryNoLaunchCommandReason:         handoff.CarryUnresolved,
	carryNothingAppliesReason:          handoff.CarryUnconfirmed,
	carryUnresumableReason:             handoff.CarryUnresumable,
	carryNoRegistryReason:              handoff.CarryUnresolved,
	carryTargetUnresolvedReason:        handoff.CarryUnresolved,
	carryTargetMisplacedReason:         handoff.CarryUnresolved,
	carrySourceMisplacedReason:         handoff.CarryUnresolved,
	carryNoAmbientClaudeReason:         handoff.CarryUnresolved,
	carryNoAmbientCodexReason:          handoff.CarryUnresolved,
	carryNoClaudeDirReason:             handoff.CarryUnresolved,
	carryTranscriptMissingReason:       handoff.CarryMissing,
	carryRolloutMissingReason:          handoff.CarryMissing,
	carryStoreUnlistedReason:           handoff.CarryUnverified,
	carrySourceStoreUnlistedReason:     handoff.CarryUnverified,
	carryTranscriptsUnlistedReason:     handoff.CarryUnverified,
	carryTranscriptCopiesReason:        handoff.CarryDuplicated,
	carryUncorrelatedReason:            handoff.CarryUnverified,
	carryRolloutMisplacedReason:        handoff.CarryUnverified,
	carryOtherFormsUnlistedReason:      handoff.CarryUnverified,
	carryOtherFormsReason:              handoff.CarryCompressed,
	carryTargetHoldsOtherRolloutReason: handoff.CarryDuplicated,
	carryForkUnreadableReason:          handoff.CarryUnverified,
	codexForkCarryReason:               handoff.CarryForked,
	codexRevertCarrySourceReason:       handoff.CarryBacktracked,
	codexRevertCarryTargetReason:       handoff.CarryBacktracked,
	staleCarryReason:                   handoff.CarryStale,
}

var carryRefusalFormats = []struct {
	re   *regexp.Regexp
	code string
}{
	{carryFormatRE(carryUnrecordedFormat), handoff.CarryUnrecorded},
	{carryFormatRE(carryForeignIDFormat), handoff.CarryUnrecorded},
	{carryFormatRE(carrySourceUnregisteredFormat), handoff.CarryUnresolved},
	{carryFormatRE(carryUnsupportedFormat), handoff.CarryUnsupported},
	{carryFormatRE(carryDuplicatedFormat), handoff.CarryDuplicated},
	{carryFormatRE(carrySymlinkFormat), handoff.CarrySymlinked},
}

var carryFormatVerbRE = regexp.MustCompile(`%[sqd]`)

// carryFormatRE matches what fmt.Sprintf renders from format, whatever it
// substitutes for each verb.
func carryFormatRE(format string) *regexp.Regexp {
	parts := carryFormatVerbRE.Split(format, -1)
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return regexp.MustCompile(`(?s)^` + strings.Join(parts, `.*`) + `$`)
}

// carryRefusalCode is reason's carry refusal code, "" for no refusal. A
// refusal with no code here — one added since — reads as
// handoff.CarryUnconfirmed: still a refusal, and still never prose.
func carryRefusalCode(reason string) string {
	if reason == "" {
		return ""
	}
	if code, ok := carryRefusalCodeOf[reason]; ok {
		return code
	}
	for _, f := range carryRefusalFormats {
		if f.re.MatchString(reason) {
			return f.code
		}
	}
	if strings.Contains(reason, carryOperatorRepairMark) {
		return handoff.CarryForeign
	}
	return handoff.CarryUnconfirmed
}
