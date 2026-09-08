package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// One diagnostic per process, rather than another warning on every creation.
// Leave undecodable journals in place: quarantining without their references
// would make a later pass wrongly classify their receipts as abandoned.
var hookReceiptReferenceWarning sync.Once

// Called with the publication lock held. No publisher can have an unreferenced
// directory here, even if its write/sync stalled past the grace period. A crash
// releases the lock, and its abandoned directory becomes eligible after grace.
func pruneUnpublishedHookReceipts(dir string, entries []os.DirEntry, now time.Time) error {
	referenced := make(map[string]bool)
	ambiguous := false
	for _, entry := range entries {
		name := entry.Name()
		if (!strings.HasPrefix(name, "progress-") && !strings.HasPrefix(name, "retired-entries-")) || !strings.HasSuffix(name, ".json") {
			continue
		}
		base, err := hookReceiptReference(filepath.Join(dir, name), entry)
		if err != nil {
			ambiguous = true
			hookReceiptReferenceWarning.Do(func() {
				log.WarningLog.Printf("cannot determine hook receipt references from %s: %v; retaining ambiguous receipts until the journal can be read", name, err)
			})
			continue
		}
		referenced[base] = true
	}
	// Unknown references protect every unreferenced directory in this pass;
	// independently valid journals are still handled by pruneHookProgress.
	if ambiguous {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "entries-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if referenced[entry.Name()] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if now.Sub(info.ModTime()) < progressGraceAge {
			continue
		}
		if _, err := withInactiveHookProgressLease(path, func() error { return os.RemoveAll(path) }); err != nil {
			return err
		}
	}
	return nil
}

func hookReceiptReference(path string, entry os.DirEntry) (string, error) {
	if !entry.Type().IsRegular() {
		return "", fmt.Errorf("journal is not regular")
	}
	data, err := BoundedReadFile(path)
	if err != nil {
		return "", err
	}
	var p hookProgress
	if err := json.Unmarshal(data, &p); err != nil {
		return "", err
	}
	base := filepath.Base(p.Directory)
	if !strings.HasPrefix(base, "entries-") {
		return "", fmt.Errorf("journal has no receipt directory")
	}
	// Preserve by basename even across equivalent home spellings.
	return base, nil
}
