package handoff

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// readFileNoFollow reads a durable-record file without following symlinks
// (the #3672 policy): a state or rails file that resolves to a link is a
// tampering signal, never a path to chase. Missing is not an error — absent
// is a real answer ("nothing recorded"), so the second return distinguishes
// it.
func readFileNoFollow(path string) (raw []byte, exists bool, err error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ENOENT {
			return nil, false, nil
		}
		if err == unix.ELOOP {
			return nil, false, fmt.Errorf("read %s: refusing to follow a symlinked record file", path)
		}
		return nil, false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	raw, err = io.ReadAll(f)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	return raw, true, nil
}

// marshalPreserving renders model for write-back while keeping every
// top-level field the model does not own: unknown keys from the original
// document pass through verbatim, so a field a LATER slice adds survives a
// rewrite by this binary — and, paired with the separate rails file, a
// field this binary adds survives a downgrade that rewrites the shared
// file under the same rule (#6776 review: the durable contract is
// read-modify-write-preserves, not read-model-write).
//
// known must list every JSON key the model can emit — including ones
// currently empty — so a field the mutation cleared leaves the file rather
// than resurrecting from the preserved copy.
func marshalPreserving[T any](model T, extras map[string]json.RawMessage, known map[string]bool, path string) ([]byte, error) {
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", path, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, fmt.Errorf("encode %s: %w", path, err)
	}
	for key, value := range extras {
		if known[key] {
			continue
		}
		if _, emitted := fields[key]; !emitted {
			fields[key] = value
		}
	}
	raw, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", path, err)
	}
	return raw, nil
}
