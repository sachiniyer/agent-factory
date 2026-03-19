package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithFileLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	called := false
	err := WithFileLock(path, func() error {
		called = true
		return nil
	})
	assert.NoError(t, err)
	assert.True(t, called)

	// Lock file should exist
	_, err = os.Stat(path + ".lock")
	assert.NoError(t, err)
}

func TestWithFileLockPropagatesError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	err := WithFileLock(path, func() error {
		return assert.AnError
	})
	assert.ErrorIs(t, err, assert.AnError)
}

func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	data := []byte(`{"key": "value"}`)
	err := AtomicWriteFile(path, data, 0644)
	require.NoError(t, err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestAtomicWriteFileCreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "test.json")

	data := []byte(`[]`)
	err := AtomicWriteFile(path, data, 0644)
	require.NoError(t, err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}
