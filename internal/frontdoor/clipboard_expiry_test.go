package frontdoor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClipboardTextReadsDoNotRenewButExplicitSaveDoes(t *testing.T) {
	clock := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	s, err := newSnippetStoreWithClock(filepath.Join(shortTestDir(t), "snippets.json"), func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	clip, err := s.create(snippetKindClip, "", "copied text", false, "phone")
	if err != nil {
		t.Fatal(err)
	}
	want := clock.Add(30 * time.Minute)
	if clip.ExpiresAt == nil || !clip.ExpiresAt.Equal(want) {
		t.Fatalf("clip expiry=%v want %v", clip.ExpiresAt, want)
	}
	clock = clock.Add(20 * time.Minute)
	_ = s.list()
	if _, err := s.precondition(clip.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := s.list()[0]; !got.ExpiresAt.Equal(want) {
		t.Fatal("read renewed expiry")
	}
	body := "edited clipboard text"
	updated, err := s.update(clip.ID, snippetUpdate{Body: &body}, 1)
	if err != nil || !updated.ExpiresAt.Equal(clock.Add(snippetClipTTL)) {
		t.Fatalf("explicit save did not renew expiry: %v %v", updated.ExpiresAt, err)
	}
	clock = want
	if len(s.list()) != 1 {
		t.Fatal("saved clip expired at its old deadline")
	}
	clock = *updated.ExpiresAt
	if len(s.list()) != 0 {
		t.Fatal("clip remained live at its renewed expiry")
	}
}

func TestClipboardOSCDuplicatePublicationRenewsCanonicalExpiry(t *testing.T) {
	clock := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	s, err := newSnippetStoreWithClock(filepath.Join(shortTestDir(t), "snippets.json"), func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.upsertOSC("same copy", "phone", nil)
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(20 * time.Minute)
	repeated, err := s.upsertOSC("same copy", "laptop", nil)
	if err != nil || !repeated.ExpiresAt.After(*first.ExpiresAt) || !repeated.UpdatedAt.After(first.UpdatedAt) || repeated.Revision != 2 || repeated.Origin != "laptop" || len(s.list()) != 1 {
		t.Fatalf("duplicate publication did not renew one canonical value: %+v %v", repeated, err)
	}
	changed, err := s.upsertOSC("new copy", "phone", nil)
	if err != nil || !changed.ExpiresAt.Equal(clock.Add(30*time.Minute)) || changed.Revision != 3 || changed.ID != snippetOSCID || !changed.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("new body expiry/identity: %+v %v", changed, err)
	}
}

func TestClipboardLegacyExpiryReclaimedOnRestartWithoutDeletingSavedText(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := base
	path := filepath.Join(shortTestDir(t), "snippets.json")
	s, err := newSnippetStoreWithClock(path, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.create(snippetKindSnippet, "Saved", "saved secret", true, "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.create(snippetKindClip, "", "expired secret", false, "")
	if err != nil {
		t.Fatal(err)
	}
	clock = base.Add(20 * time.Minute)
	fresh, err := s.create(snippetKindClip, "", "fresh secret", false, "")
	if err != nil {
		t.Fatal(err)
	}
	osc, err := s.upsertOSC("latest OSC body", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	osc.CreatedAt = base.Add(-time.Hour)
	items := []SnippetRecord{saved, old, fresh, osc}
	encoded, err := json.Marshal(snippetStoreFile{Version: 1, Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	clock = base.Add(31 * time.Minute)
	reopened, err := newSnippetStoreWithClock(path, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	views := reopened.list()
	if len(views) != 3 {
		t.Fatalf("restart records=%d want saved + fresh + OSC", len(views))
	}
	for _, view := range views {
		if view.ID == saved.ID && (view.ExpiresAt != nil || view.Body != saved.Body || !view.Pinned) {
			t.Fatal("saved record changed")
		}
		if view.Kind == snippetKindClip && !view.ExpiresAt.Equal(base.Add(50*time.Minute)) {
			t.Fatalf("legacy clip deadline changed: %+v", view)
		}
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored snippetStoreFile
	if err := json.Unmarshal(onDisk, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Items) != 4 {
		t.Fatalf("expired body still on disk: %d records", len(stored.Items))
	}
}
