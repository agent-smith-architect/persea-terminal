package frontdoor

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"persea-terminal/internal/proto"
)

// A deliberate expiry choice replaces the policy. Duplicate additions and
// editor saves have separate renewal tests that preserve the longer policy.
func TestClipboardExplicitExpiryReplacesDuration(t *testing.T) {
	policies := []int{1800, 14400, 86400, 604800, 2592000, 0}
	for _, from := range policies {
		for _, to := range policies {
			t.Run(fmt.Sprintf("%d_to_%d", from, to), func(t *testing.T) {
				clock := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
				path := filepath.Join(shortTestDir(t), "snippets.json")
				text, err := newSnippetStoreWithClock(path, func() time.Time { return clock })
				if err != nil {
					t.Fatal(err)
				}
				imageDir := filepath.Join(shortTestDir(t), clipboardImageDirectory)
				images, err := newClipboardImageStore(imageDir, proto.MaxImage, func() time.Time { return clock })
				if err != nil {
					t.Fatal(err)
				}
				defer images.close()
				payload := oraclePNG(t, 2, 3)
				image, err := images.create("image/png", payload, "", from)
				if err != nil {
					t.Fatal(err)
				}
				records := []SnippetRecord{}
				for _, kind := range []string{snippetKindClip, snippetKindSnippet} {
					label := ""
					if kind == snippetKindSnippet {
						label = "Saved text"
					}
					r, err := text.create(kind, label, "expiry "+kind, false, "", from)
					if err != nil {
						t.Fatal(err)
					}
					records = append(records, r)
				}
				for attempt := 0; attempt < 2; attempt++ {
					clock = clock.Add(10 * time.Minute)
					for i, old := range records {
						updated, err := text.update(old.ID, snippetUpdate{RetentionSeconds: intPtr(to)}, old.Revision)
						if err != nil {
							t.Fatal(err)
						}
						if updated.ID != old.ID || updated.Revision != old.Revision+1 || updated.RetentionSeconds != to || !updated.UpdatedAt.Equal(clock) || !updated.CreatedAt.Equal(old.CreatedAt) {
							t.Fatalf("explicit text policy not replaced: from=%d to=%d got=%+v", from, to, updated)
						}
						if to == 0 {
							if updated.ExpiresAt != nil {
								t.Fatal("text No expiry did not clear deadline")
							}
						} else if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(clock.Add(time.Duration(to)*time.Second)) {
							t.Fatal("text TTL must start from this selection, never the previous deadline")
						}
						if _, err := text.update(old.ID, snippetUpdate{RetentionSeconds: intPtr(from)}, old.Revision); !errors.Is(err, errSnippetConflict) {
							t.Fatal("stale text expiry accepted", err)
						}
						records[i] = updated
					}
					old := image
					image, err = images.updateRetention(old.ID, to, old.Revision)
					if err != nil {
						t.Fatal(err)
					}
					if image.ID != old.ID || image.Revision != old.Revision+1 || image.RetentionSeconds != to || !image.UpdatedAt.Equal(clock) || !image.CreatedAt.Equal(old.CreatedAt) {
						t.Fatalf("explicit image policy not replaced: %+v", image)
					}
					if to == 0 {
						if !image.ExpiresAt.IsZero() {
							t.Fatal("image No expiry did not clear deadline")
						}
					} else if !image.ExpiresAt.Equal(clock.Add(time.Duration(to) * time.Second)) {
						t.Fatal("image TTL must start from this selection, never the previous deadline")
					}
					if _, err := images.updateRetention(old.ID, from, old.Revision); !errors.Is(err, errClipboardImageConflict) {
						t.Fatal("stale image expiry accepted", err)
					}
				}
				reopened, err := newSnippetStoreWithClock(path, func() time.Time { return clock })
				if err != nil {
					t.Fatal(err)
				}
				for _, record := range records {
					got, err := reopened.precondition(record.ID, record.Revision)
					if err != nil || got.RetentionSeconds != to || (got.ExpiresAt == nil) != (record.ExpiresAt == nil) || (got.ExpiresAt != nil && !got.ExpiresAt.Equal(*record.ExpiresAt)) {
						t.Fatal("explicit text expiry did not survive reopening", err)
					}
				}
				reopenedImages, err := newClipboardImageStore(imageDir, proto.MaxImage, func() time.Time { return clock })
				if err != nil {
					t.Fatal(err)
				}
				defer reopenedImages.close()
				got, body, err := reopenedImages.get(image.ID)
				if err != nil || got != image || !bytes.Equal(body, payload) {
					t.Fatal("explicit image expiry did not survive reopening", err)
				}
			})
		}
	}
}
