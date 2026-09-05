package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStoreAt(t.TempDir())
}

func TestCreateSessionNumbering(t *testing.T) {
	s := newTestStore(t)
	a, err := s.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	if a.Num != 1 || b.Num != 2 {
		t.Fatalf("session numbers = %d, %d; want 1, 2", a.Num, b.Num)
	}
	if a.ID == b.ID {
		t.Fatalf("two sessions share id %q", a.ID)
	}
	// The created ledger holds one line per session in "<ts> <num> <id>" form.
	data, err := os.ReadFile(filepath.Join(s.Root(), "created"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("created lines = %d, want 2: %q", len(lines), data)
	}
	parts := strings.Fields(lines[1])
	if len(parts) != 3 || parts[1] != "2" || parts[2] != b.ID {
		t.Fatalf("created line = %q, want '<ts> 2 %s'", lines[1], b.ID)
	}
}

func TestEnsureSession(t *testing.T) {
	s := newTestStore(t)
	// An id with no ledger entry gets one.
	m, err := s.EnsureSession("sfix")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "sfix" || m.Num != 1 {
		t.Fatalf("EnsureSession = %+v, want id sfix num 1", m)
	}
	// The same id returns the existing meta without a second ledger line.
	again, err := s.EnsureSession("sfix")
	if err != nil {
		t.Fatal(err)
	}
	if again.Num != 1 || again.ID != "sfix" {
		t.Fatalf("EnsureSession again = %+v", again)
	}
}

func TestListSessionsOrderedAndNamed(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.CreateSession()
	b, _ := s.CreateSession()
	if err := s.SaveName(b.ID, "build tooling"); err != nil {
		t.Fatal(err)
	}
	metas, err := s.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 || metas[0].ID != a.ID || metas[1].ID != b.ID {
		t.Fatalf("ListSessions = %+v", metas)
	}
	if metas[1].Name != "build tooling" {
		t.Fatalf("session b name = %q, want %q", metas[1].Name, "build tooling")
	}
}

func TestTouchUpdatedDedupes(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.CreateSession()
	b, _ := s.CreateSession()
	if err := s.TouchUpdated(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchUpdated(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchUpdated(b.ID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.Root(), "updated"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("updated lines = %d, want 2 (consecutive same-session updates merged): %q", len(lines), data)
	}
	if !strings.HasSuffix(lines[0], " "+a.ID) || !strings.HasSuffix(lines[1], " "+b.ID) {
		t.Fatalf("updated lines = %q", data)
	}
	if got := s.LastUpdatedID(); got != b.ID {
		t.Fatalf("LastUpdatedID = %q, want %q", got, b.ID)
	}
	// A third consecutive update on b is merged again.
	if err := s.TouchUpdated(b.ID); err != nil {
		t.Fatal(err)
	}
	if lines2, _ := os.ReadFile(filepath.Join(s.Root(), "updated")); strings.Count(string(lines2), "\n") != 2 {
		t.Fatalf("updated lines after repeat = %q, want 2", lines2)
	}
}

func TestStoreNoHomeFallback(t *testing.T) {
	// NewStore without a home directory errors (no persistence); the store
	// helpers on a missing dir return empty results rather than panicking.
	s := newTestStore(t)
	if metas, err := s.ListSessions(); err != nil || len(metas) != 0 {
		t.Fatalf("empty store ListSessions = %+v, %v", metas, err)
	}
	if got := s.LastUpdatedID(); got != "" {
		t.Fatalf("empty store LastUpdatedID = %q, want empty", got)
	}
}

// UpdatedTimes maps each session to its latest updated-ledger timestamp;
// sessions never recorded in the ledger are absent from the map.
func TestUpdatedTimes(t *testing.T) {
	s := newTestStore(t)
	a, err := s.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.UpdatedTimes(); len(got) != 0 {
		t.Fatalf("fresh store UpdatedTimes = %v, want empty", got)
	}
	for _, id := range []string{a.ID, b.ID, a.ID} {
		if err := s.TouchUpdated(id); err != nil {
			t.Fatal(err)
		}
	}
	got := s.UpdatedTimes()
	if len(got) != 2 {
		t.Fatalf("UpdatedTimes = %v, want 2 entries", got)
	}
	if got[a.ID] < got[b.ID] {
		t.Fatalf("a ts %d earlier than b ts %d after re-touch", got[a.ID], got[b.ID])
	}
	if _, ok := got[c.ID]; ok {
		t.Fatalf("untouched session %s in UpdatedTimes: %v", c.ID, got)
	}
}
