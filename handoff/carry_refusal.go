package handoff

// Carry refusal codes are the "carry" param of a carry-refused event: why
// require_carry found that the session's conversation would not be carried.
// They are af's own closed vocabulary. The carry's refusal prose can embed a
// path or an account name, so it never reaches an event, the shadow log or a
// report_to delivery (#7156 review P3-2).
const (
	// CarryUnrecorded: af holds no recorded conversation of the session's own
	// provider — the in-memory precondition's own meaning.
	CarryUnrecorded = "unrecorded"
	// CarryRemoteWorkspace: the session runs in a remote workspace, and the
	// carry copies between local account homes.
	CarryRemoteWorkspace = "remote-workspace"
	// CarryUnsupported: the agent's conversations cannot be carried.
	CarryUnsupported = "unsupported"
	// CarryUnresolved: af could not resolve the home, account or directory the
	// conversation is filed under.
	CarryUnresolved = "unresolved"
	// CarryUnverified: af could not read what it needs to confirm the carry —
	// a provider store, a listing, or a header.
	CarryUnverified = "unverified"
	// CarryMissing: the conversation's file is missing from its home.
	CarryMissing = "missing"
	// CarryDuplicated: more than one file records the conversation.
	CarryDuplicated = "duplicated"
	// CarryCompressed: the provider holds the conversation only in forms the
	// carry does not copy (compressed, or only in thread/revert rollouts).
	CarryCompressed = "compressed"
	// CarryBacktracked: the conversation continues in a thread/revert rollout.
	CarryBacktracked = "backtracked"
	// CarryForked: the conversation is a fork whose history its parent holds.
	CarryForked = "forked"
	// CarryStale: the recorded conversation is no longer the newest one in its
	// worktree.
	CarryStale = "stale"
	// CarrySymlinked: a symbolic link lies on the conversation's path.
	CarrySymlinked = "symlinked"
	// CarryUnresumable: the launch command cannot resume a named conversation.
	CarryUnresumable = "unresumable"
	// CarryForeign: an operator-recorded conversation af could not confirm is
	// the session's own.
	CarryForeign = "foreign"
	// CarryUnconfirmed: no answer confirmed the carry — no check was available,
	// or it refused with a value outside this vocabulary.
	CarryUnconfirmed = "unconfirmed"
)

var carryRefusalCodes = map[string]bool{
	CarryUnrecorded: true, CarryRemoteWorkspace: true, CarryUnsupported: true,
	CarryUnresolved: true, CarryUnverified: true, CarryMissing: true,
	CarryDuplicated: true, CarryCompressed: true, CarryBacktracked: true,
	CarryForked: true, CarryStale: true, CarrySymlinked: true, CarryUnresumable: true,
	CarryForeign: true, CarryUnconfirmed: true,
}

// carryRefusalParam is the carry-refused event's "carry" param for a
// session's refusal code. An empty code is the in-memory precondition's
// refusal; anything outside the vocabulary reads as CarryUnconfirmed, so
// nothing but a code ever reaches the param.
func carryRefusalParam(code string) string {
	switch {
	case code == "":
		return CarryUnrecorded
	case carryRefusalCodes[code]:
		return code
	}
	return CarryUnconfirmed
}
