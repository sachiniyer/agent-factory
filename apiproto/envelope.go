// Package apiproto holds the neutral JSON wire types shared by the `af` CLI
// (api/) and the daemon-hosted HTTP server (daemon/). It deliberately depends on
// neither: api/ imports daemon/ for its RPC client wrappers, so the daemon
// cannot import api/, and both instead import this leaf package. Keeping the
// Envelope definition here — rather than duplicated in each — is what guarantees
// the CLI's `--json` output and the HTTP server's responses stay byte-for-byte
// identical (#1029 PR 4).
package apiproto

import (
	"encoding/json"
	"fmt"
	"io"
)

// Envelope is the additive JSON wrapper shared by the CLI and the daemon-hosted
// HTTP server. A success carries the payload in Data with Error nil; a failure
// carries a nil Data and a populated Error. Both members always serialize (no
// omitempty) so consumers can branch on `error === null` without a presence
// check:
//
//	success: {"data": <payload>, "error": null}
//	failure: {"data": null, "error": {"message": "<msg>"}}
//
// On the CLI it is NEW infrastructure: today's commands still emit the bare
// payload by default, and the envelope is only produced on the explicit opt-in
// path (the --json flag), so no existing command's stdout/stderr bytes change.
// The HTTP server always emits it.
type Envelope struct {
	Data  any            `json:"data"`
	Error *EnvelopeError `json:"error"`
}

// EnvelopeError is the error member of an Envelope. Message is a human-readable
// description; the struct leaves room to grow additional fields (e.g. a machine
// code) later without breaking the additive contract.
type EnvelopeError struct {
	Message string `json:"message"`
}

// Success wraps a payload as a successful Envelope (Error nil).
func Success(data any) Envelope {
	return Envelope{Data: data, Error: nil}
}

// Failure wraps a message as a failed Envelope (Data nil).
func Failure(msg string) Envelope {
	return Envelope{Data: nil, Error: &EnvelopeError{Message: msg}}
}

// MarshalIndented is the single JSON-encoding primitive the CLI and HTTP output
// paths share so bare and enveloped output format identically (two-space
// indent).
func MarshalIndented(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// WriteEnvelope encodes env and writes it, newline-terminated, to w. It is the
// shared write path the CLI's opt-in --json output and the daemon HTTP server
// both reuse, keeping their responses byte-for-byte consistent.
func WriteEnvelope(w io.Writer, env Envelope) error {
	data, err := MarshalIndented(env)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
