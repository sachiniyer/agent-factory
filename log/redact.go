package log

import (
	"io"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/internal/credscrub"
)

// credentialRedactingWriter is a last-line defense for agent-factory.log and its
// stderr fallback. URL-producing call sites still redact structurally so
// returned errors and non-log surfaces are safe too.
//
// Two passes, because they cover different things and neither subsumes the
// other. RedactAccessTokenText is key-aware: it knows `access_token` is a
// credential whatever the value looks like. credscrub.Scrub is shape-aware: it
// recognizes a GitHub PAT or a PEM block with no key in sight, which is how a
// credential arrives here in practice — folded into a git stderr, an operator's
// docker run_args, or an echoed Authorization header.
//
// Shape pass FIRST, so the key-aware pass has the last word on its own key.
// The other order rewrote `access_token=REDACTED` into
// `access_token=[redacted-secret]`, because the shape pass sees a credential
// key with a value and cannot know REDACTED is another redactor's marker.
// Either order removes the secret; only this one keeps the documented output.
//
// Until #2884 only the first pass ran, so the bug-report bundle scrubbed eight
// credential shapes this file wrote to disk in cleartext. Sharing the patterns
// with bugreport is what keeps the two from drifting again.
type credentialRedactingWriter struct{ writer io.Writer }

func (w credentialRedactingWriter) Write(p []byte) (int, error) {
	redacted := []byte(agentproto.RedactAccessTokenText(credscrub.Scrub(string(p))))
	n, err := w.writer.Write(redacted)
	if err != nil {
		return 0, err
	}
	if n != len(redacted) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

func redactedLogSink(writer io.Writer) io.Writer {
	return credentialRedactingWriter{writer: writer}
}
