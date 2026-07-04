package api

import (
	"encoding/json"
	"fmt"
	"io"
)

// Envelope is the additive JSON wrapper shared by the CLI and the (later,
// #1029 PR 4+) daemon-hosted HTTP server. A success carries the payload in Data
// with Error nil; a failure carries a nil Data and a populated Error. Both
// members always serialize (no omitempty) so consumers can branch on
// `error === null` without a presence check:
//
//	success: {"data": <payload>, "error": null}
//	failure: {"data": null, "error": {"message": "<msg>"}}
//
// It is NEW infrastructure: today's commands still emit the bare payload by
// default, and the envelope is only produced on the explicit opt-in path
// (the --json flag), so no existing command's stdout/stderr bytes change.
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

// successEnvelope wraps a payload as a successful Envelope (Error nil).
func successEnvelope(data any) Envelope {
	return Envelope{Data: data, Error: nil}
}

// errorEnvelope wraps a message as a failed Envelope (Data nil).
func errorEnvelope(msg string) Envelope {
	return Envelope{Data: nil, Error: &EnvelopeError{Message: msg}}
}

// marshalIndented is the single JSON-encoding primitive the output helpers
// share so bare and enveloped output format identically (two-space indent).
func marshalIndented(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// writeEnvelope encodes env and writes it, newline-terminated, to w. It is the
// shared write path the opt-in --json output uses today and the HTTP server
// will reuse, keeping the CLI and HTTP responses byte-for-byte consistent.
func writeEnvelope(w io.Writer, env Envelope) error {
	data, err := marshalIndented(env)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
