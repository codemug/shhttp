package id

import (
	"testing"
	"time"
)

func TestNewIsValidAndSortable(t *testing.T) {
	a := New(Session)
	if !Valid(Session, a) {
		t.Fatalf("invalid id %q", a)
	}
	if Valid(Key, a) {
		t.Fatalf("%q should not be a valid key id", a)
	}
	t0 := time.UnixMilli(1_700_000_000_000)
	if x, y := ULID(t0), ULID(t0.Add(time.Millisecond)); x >= y {
		t.Fatalf("not sortable: %s >= %s", x, y)
	}
	for _, bad := range []string{"ses_", "ses_../../etc/passwd", "ses_0123456789abcdefghjkmnpqrsTV", "x" + a} {
		if Valid(Session, bad) {
			t.Errorf("Valid(%q) = true", bad)
		}
	}
}
