package store

import (
	"path/filepath"
	"testing"
)

// newFencedTestStore opens a fresh store in a temp dir and applies the fence.
func newFencedTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(FallbackConfig(filepath.Join(t.TempDir(), "local")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestApplyLocalIDFence_FirstObservationIsAboveFence is the core guarantee the
// whole local/global id-routing scheme rests on.
func TestApplyLocalIDFence_FirstObservationIsAboveFence(t *testing.T) {
	s := newFencedTestStore(t)

	warning, err := s.ApplyLocalIDFence()
	if err != nil {
		t.Fatalf("ApplyLocalIDFence: %v", err)
	}
	if warning != "" {
		t.Fatalf("fresh store should not warn, got %q", warning)
	}

	if err := s.CreateSession("s1", "p", "d"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	id, err := s.AddObservation(AddObservationParams{
		SessionID: "s1", Type: "manual", Title: "fenced", Content: "body",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	if id != LocalIDFence+1 {
		t.Fatalf("first fenced id = %d, want %d", id, LocalIDFence+1)
	}
	if !IsLocalID(id) {
		t.Fatalf("IsLocalID(%d) = false, want true", id)
	}
}

// TestApplyLocalIDFence_FencedRowIsSearchable guards the external-content FTS5
// table: observations_fts uses content_rowid='id', so a billion-scale rowid must
// still round-trip through search.
func TestApplyLocalIDFence_FencedRowIsSearchable(t *testing.T) {
	s := newFencedTestStore(t)
	if _, err := s.ApplyLocalIDFence(); err != nil {
		t.Fatalf("ApplyLocalIDFence: %v", err)
	}
	if err := s.CreateSession("s1", "proj", "d"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	id, err := s.AddObservation(AddObservationParams{
		SessionID: "s1", Type: "manual", Title: "needle in local store", Content: "body", Project: "proj",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	results, err := s.Search("needle", SearchOptions{Project: "proj", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("fenced observation not returned by FTS5 search")
	}
	if results[0].ID != id {
		t.Fatalf("search returned id %d, want %d", results[0].ID, id)
	}

	got, err := s.GetObservation(id)
	if err != nil {
		t.Fatalf("GetObservation(%d): %v", id, err)
	}
	if got.Title != "needle in local store" {
		t.Fatalf("GetObservation title = %q", got.Title)
	}
}

// TestApplyLocalIDFence_Idempotent — re-applying must not perturb the sequence,
// because the fence is re-applied on every open of a local store.
func TestApplyLocalIDFence_Idempotent(t *testing.T) {
	s := newFencedTestStore(t)
	if _, err := s.ApplyLocalIDFence(); err != nil {
		t.Fatalf("first ApplyLocalIDFence: %v", err)
	}
	if err := s.CreateSession("s1", "p", "d"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	firstID, err := s.AddObservation(AddObservationParams{SessionID: "s1", Type: "manual", Title: "a", Content: "a"})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	warning, err := s.ApplyLocalIDFence()
	if err != nil {
		t.Fatalf("second ApplyLocalIDFence: %v", err)
	}
	if warning != "" {
		t.Fatalf("already-fenced store should not warn, got %q", warning)
	}

	secondID, err := s.AddObservation(AddObservationParams{SessionID: "s1", Type: "manual", Title: "b", Content: "b"})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if secondID != firstID+1 {
		t.Fatalf("ids not contiguous after re-fencing: %d then %d", firstID, secondID)
	}
}

// TestApplyLocalIDFence_WarnsOnPreExistingRows — a store that accumulated rows
// before being fenced keeps ambiguous ids, and must say so.
func TestApplyLocalIDFence_WarnsOnPreExistingRows(t *testing.T) {
	s := newFencedTestStore(t)
	if err := s.CreateSession("s1", "p", "d"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	preID, err := s.AddObservation(AddObservationParams{SessionID: "s1", Type: "manual", Title: "pre", Content: "pre"})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if IsLocalID(preID) {
		t.Fatalf("unfenced id %d should be below the fence", preID)
	}

	warning, err := s.ApplyLocalIDFence()
	if err != nil {
		t.Fatalf("ApplyLocalIDFence: %v", err)
	}
	if warning == "" {
		t.Fatal("expected a warning about pre-fence rows")
	}

	postID, err := s.AddObservation(AddObservationParams{SessionID: "s1", Type: "manual", Title: "post", Content: "post"})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if !IsLocalID(postID) {
		t.Fatalf("post-fence id %d should be above the fence", postID)
	}
}

// TestIsLocalID_Boundary pins the exact boundary so a later refactor cannot
// shift it by one and silently misroute an id.
func TestIsLocalID_Boundary(t *testing.T) {
	cases := []struct {
		id   int64
		want bool
	}{
		{1, false},
		{LocalIDFence - 1, false},
		{LocalIDFence, false},
		{LocalIDFence + 1, true},
	}
	for _, tc := range cases {
		if got := IsLocalID(tc.id); got != tc.want {
			t.Errorf("IsLocalID(%d) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
