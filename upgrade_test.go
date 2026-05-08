package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func TestExtractBinaryFromTarGz(t *testing.T) {
	archive := makeTarGz(t, map[string][]byte{
		"agent-factory": []byte("binary-content"),
		"README":        []byte("not the binary"),
	})

	got, err := extractBinaryFromTarGz(bytes.NewReader(archive), "agent-factory")
	if err != nil {
		t.Fatalf("extractBinaryFromTarGz returned error: %v", err)
	}
	if string(got) != "binary-content" {
		t.Fatalf("extracted %q, want binary-content", string(got))
	}
}

func TestExtractBinaryFromTarGzNestedPath(t *testing.T) {
	archive := makeTarGz(t, map[string][]byte{
		"dist/agent-factory": []byte("nested-binary"),
	})

	got, err := extractBinaryFromTarGz(bytes.NewReader(archive), "agent-factory")
	if err != nil {
		t.Fatalf("extractBinaryFromTarGz returned error: %v", err)
	}
	if string(got) != "nested-binary" {
		t.Fatalf("extracted %q, want nested-binary", string(got))
	}
}

func TestExtractBinaryFromTarGzMissingBinary(t *testing.T) {
	archive := makeTarGz(t, map[string][]byte{
		"other": []byte("content"),
	})

	_, err := extractBinaryFromTarGz(bytes.NewReader(archive), "agent-factory")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestExtractBinaryFromTarGzInvalidGzip(t *testing.T) {
	_, err := extractBinaryFromTarGz(strings.NewReader("not gzip"), "agent-factory")
	if err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("expected gzip error, got %v", err)
	}
}

func makeTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0755,
			Size: int64(len(data)),
		}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar data: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}
