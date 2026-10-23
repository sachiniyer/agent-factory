package proposal

import (
	"strings"
	"testing"
)

func plantAttachment(t *testing.T, s *Store, sessionID, slug string) {
	t.Helper()
	blob, err := s.git([]byte(slug+"\n"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.git(nil, "update-ref", attachRefPrefix+sessionID, strings.TrimSpace(blob)); err != nil {
		t.Fatal(err)
	}
}

func TestNonBlobAttachmentCanBeRepaired(t *testing.T) {
	for _, target := range []string{"HEAD", "HEAD^{tree}"} {
		s, _ := newTestStore(t)
		startAttached(t, s, "p", sessA)
		id, err := s.git(nil, "rev-parse", target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.git(nil, "update-ref", attachRefPrefix+sessA, strings.TrimSpace(id)); err != nil {
			t.Fatal(err)
		}
		if err := s.Attach("p", sessA); err != nil {
			t.Fatalf("attach over a %s attachment: %v", target, err)
		}
		if _, err := s.git(nil, "update-ref", attachRefPrefix+sessA, strings.TrimSpace(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Detach(sessA); err != nil {
			t.Fatalf("detach of a %s attachment: %v", target, err)
		}
	}
}

func TestInvalidAttachmentSlugIsRefused(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	startAttached(t, s, "q", sessB)
	editOnDisk(t, s, "q", "goals.md", "pending edit of q\n")
	plantAttachment(t, s, sessA, ".")

	if _, err := s.Checkpoint(sessA); err == nil {
		t.Fatal("Checkpoint used an attachment that names no valid proposal")
	}
	if log, _ := s.git(nil, "log", "--format=%H", "-1", "--", "q/goals.md"); log != "" {
		if body, _ := s.git(nil, "log", "-1", "--format=%B", "--", "q/goals.md"); strings.Contains(body, sessA) {
			t.Fatalf("session A committed q's edits through a corrupt attachment:\n%s", body)
		}
	}
	got, err := s.Attachments()
	if err != nil {
		t.Fatalf("Attachments failed on one corrupt attachment: %v", err)
	}
	if _, ok := got[sessA]; ok {
		t.Fatalf("Attachments listed the corrupt attachment: %v", got)
	}
	if err := s.Attach("p", sessA); err != nil {
		t.Fatalf("re-attaching over a corrupt attachment failed: %v", err)
	}
}
