package config

import "crypto/sha256"

// ConfigDigest identifies the exact config.toml bytes one operation touched: the
// bytes a save wrote, or the bytes a load parsed. It exists so a save can answer
// one question without reading anything back — did the daemon's reload load THIS
// save's file, or did the file move underneath it? (#4247)
//
// Comparing BYTES is what makes that answer cheap. The post-apply readback this
// replaced compared VALUES, which forced it to choose which store to re-read,
// normalise every accepted duration spelling, decide which generation it had
// resolved, and handle a file that would no longer load — four problems that were
// all consequences of reading, not of the question being asked. A digest has none
// of them: each side is captured by the write and by the load themselves, so
// there is no second read to get wrong.
//
// It also catches a hand-edit, which no cooperating counter can. A write
// generation bumped under the config lock would miss the editor that takes no
// lock, and would depend on every lock-taking writer remembering to bump it; any
// byte change changes this, and no other writer has to cooperate.
//
// The fields are unexported ON PURPOSE, and that is a contract rather than a
// style choice. net/rpc encodes with gob, which sends every EXPORTED field
// whatever its json tag says, so a digest parked on a type that crosses the
// control socket would silently become part of the wire contract — and
// config.SetResult, the obvious place to hang "a fact about this save", is
// exactly such a type, as well as being the `af config set --json` payload. This
// digest is process-local by construction: it rides BESIDE SetResult in Go and
// never inside it, and the unexported fields are what keep that true if someone
// later adds it to a request or response struct.
type ConfigDigest struct {
	known bool
	sum   [sha256.Size]byte
}

// digestConfigBytes is the one hash in the codebase. Both sides of every
// comparison come from here, so they cannot disagree about WHAT is hashed: the
// whole file, exactly as written or exactly as read, with no canonicalisation,
// trimming or re-encoding in between.
func digestConfigBytes(data []byte) ConfigDigest {
	return ConfigDigest{known: true, sum: sha256.Sum256(data)}
}

// Known reports whether this digest records any bytes. The zero value does not,
// and that state is reachable in two ways that mean the same thing to a caller:
// a load that never reached the canonical config.toml read (it materialized
// defaults or converted a legacy config.json instead), and a caller that has no
// digest to offer at all — the version-skewed fallback, whose daemon predates
// this field entirely.
func (d ConfigDigest) Known() bool { return d.known }

// Matches reports whether both digests are known AND identical. An unknown
// digest never matches, including against another unknown one: "I did not record
// any bytes" is not evidence that two files agree, and treating it as a match is
// precisely how a save would come to claim a value it cannot support.
func (d ConfigDigest) Matches(other ConfigDigest) bool {
	return d.known && other.known && d.sum == other.sum
}
