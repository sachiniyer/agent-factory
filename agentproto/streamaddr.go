package agentproto

// The /v1/sessions/{id}/stream and /stream-info routes accept three session
// address shapes. With repo_id the {id} segment is a title in that repo. Without
// it the segment is tried as a stable id and then, for legacy Web callers, as a
// title. A client that holds a stable id and must never reach any other session
// sends StreamAddressQueryParam=StreamAddressByID: the segment is then a stable
// id only, and a miss is refused rather than reinterpreted as a title (#4760
// review) — so a deleted session's id cannot open a stream to an unrelated
// session whose title happens to spell those bytes.
const (
	StreamAddressQueryParam = "by"
	StreamAddressByID       = "id"
)
