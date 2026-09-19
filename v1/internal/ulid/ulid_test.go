package ulid

import (
	"sort"
	"testing"
	"time"
)

func TestSortsByTime(t *testing.T) {
	// The reason for choosing ULID over UUIDv4: lexicographic order is
	// chronological order, so "recent deploys" is a key range scan.
	base := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 50; i++ {
		ids = append(ids, NewAt(base.Add(time.Duration(i)*time.Millisecond)).String())
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ULIDs generated in time order must be in lexicographic order")
	}
}

func TestMonotonicWithinAMillisecond(t *testing.T) {
	// Without incrementing the random component, two deploys created in the
	// same millisecond sort arbitrarily, and "the latest deployment" becomes
	// a coin flip.
	at := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 1000; i++ {
		ids = append(ids, NewAt(at).String())
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("id %d (%s) does not sort after %s", i, ids[i], ids[i-1])
		}
	}
}

func TestRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 18, 12, 34, 56, 0, time.UTC)
	id := NewAt(at)
	s := id.String()
	if len(s) != Length {
		t.Fatalf("length = %d", len(s))
	}
	back, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	if back != id {
		t.Fatalf("round trip changed the id: %x -> %x", id, back)
	}
	if got := back.Time(); !got.Equal(at.Truncate(time.Millisecond)) {
		t.Errorf("time = %v, want %v", got, at)
	}
}

func TestCrockfordConfusablesDecode(t *testing.T) {
	// Deployment ids get read off a dashboard and typed into `orch logs`.
	id := New()
	s := id.String()
	if _, err := Parse(s); err != nil {
		t.Fatal(err)
	}
	// Lowercase is accepted.
	lower := []byte(s)
	for i := range lower {
		if lower[i] >= 'A' && lower[i] <= 'Z' {
			lower[i] |= 0x20
		}
	}
	back, err := Parse(string(lower))
	if err != nil {
		t.Fatalf("lowercase should parse: %v", err)
	}
	if back != id {
		t.Error("lowercase should decode to the same id")
	}
}

func TestEncodingHasNoConfusableCharacters(t *testing.T) {
	for _, bad := range []byte{'I', 'L', 'O', 'U'} {
		for i := 0; i < len(encoding); i++ {
			if encoding[i] == bad {
				t.Errorf("encoding contains confusable %q", bad)
			}
		}
	}
}

func TestInvalidInput(t *testing.T) {
	for _, s := range []string{"", "short", "!!!!!!!!!!!!!!!!!!!!!!!!!!"} {
		if Valid(s) {
			t.Errorf("%q should not be valid", s)
		}
	}
}
