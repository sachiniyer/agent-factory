package api

import (
	"io"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// The {data,error} JSON envelope shared by the CLI's opt-in --json output and
// the daemon-hosted HTTP server (#1029 PR 4) lives in the neutral apiproto
// package so both emit a byte-identical shape. These thin package-local wrappers
// preserve the original call sites in this package while delegating to the
// single canonical definition.

func successEnvelope(data any) apiproto.Envelope {
	return apiproto.Success(data)
}

func errorEnvelope(msg string) apiproto.Envelope {
	return apiproto.Failure(msg)
}

func marshalIndented(v any) ([]byte, error) {
	return apiproto.MarshalIndented(v)
}

func writeEnvelope(w io.Writer, env apiproto.Envelope) error {
	return apiproto.WriteEnvelope(w, env)
}
