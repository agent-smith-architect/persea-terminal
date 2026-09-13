package frontdoor

import (
	"context"
	"time"
)

const clipboardReapInterval = time.Minute

// Run owns this loop and its cancellation. Expired secrets are removed even
// with no connected browser; request-time expiry still refuses them exactly
// at the deadline. Disk reclamation follows within one maintenance interval.
func (s *Server) maintainClipboard(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if s.snippets != nil {
				if err := s.snippets.reap(); err != nil {
					frontLogf("component=frontdoor event=clipboard_reap_failed store=text")
				}
			}
			if s.clipboardImages != nil {
				if err := s.clipboardImages.reap(); err != nil {
					frontLogf("component=frontdoor event=clipboard_reap_failed store=images")
				}
			}
		}
	}
}
