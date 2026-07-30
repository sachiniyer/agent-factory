package log

import (
	"io"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// accessTokenRedactingWriter is a last-line defense for agent-factory.log and
// its stderr fallback. URL-producing call sites still redact structurally so
// returned errors and non-log surfaces are safe too.
type accessTokenRedactingWriter struct{ writer io.Writer }

func (w accessTokenRedactingWriter) Write(p []byte) (int, error) {
	redacted := []byte(agentproto.RedactAccessTokenText(string(p)))
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
	return accessTokenRedactingWriter{writer: writer}
}
