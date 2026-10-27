package proposal

import "testing"

func TestServiceAttachAndDetachRepairACorruptAttachment(t *testing.T) {
	svc, _, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	plantAttachment(t, store, sessA, ".")
	if _, err := svc.Attach(sessA, "p"); err != nil {
		t.Fatalf("attach over a corrupt attachment: %v", err)
	}
	if slug, err := store.Attached(sessA); err != nil || slug != "p" {
		t.Fatalf("after attach: %q, %v", slug, err)
	}
	plantAttachment(t, store, sessA, ".")
	res, err := svc.Detach(sessA)
	if err != nil || !res.Detached {
		t.Fatalf("detach of a corrupt attachment = %+v, %v", res, err)
	}
	if slug, err := store.Attached(sessA); err != nil || slug != "" {
		t.Fatalf("after detach: %q, %v", slug, err)
	}
}
