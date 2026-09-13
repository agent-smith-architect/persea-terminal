package frontdoor

import (
	"bytes"
	"encoding/json"
	"time"
)

const clipboardDefaultRetention = 1800

type clipboardRetentionInput struct {
	Value   int
	Present bool
}

func (v *clipboardRetentionInput) UnmarshalJSON(body []byte) error {
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return errSnippetValidation
	}
	if err := json.Unmarshal(body, &v.Value); err != nil || !validClipboardRetention(v.Value) {
		return errSnippetValidation
	}
	v.Present = true
	return nil
}

func (v clipboardRetentionInput) pointer() *int {
	if !v.Present {
		return nil
	}
	n := v.Value
	return &n
}

type clipboardRevisionInput struct {
	Value   uint64
	Present bool
}

func (v *clipboardRevisionInput) UnmarshalJSON(body []byte) error {
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return errSnippetValidation
	}
	if err := json.Unmarshal(body, &v.Value); err != nil || v.Value == 0 {
		return errSnippetValidation
	}
	v.Present = true
	return nil
}

func validClipboardRetention(seconds int) bool {
	switch seconds {
	case 0, 1800, 14400, 86400, 604800, 2592000:
		return true
	}
	return false
}

func longerClipboardRetention(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	if a > b {
		return a
	}
	return b
}

func clipboardExpiry(now time.Time, seconds int, previous *time.Time) *time.Time {
	if seconds == 0 {
		return nil
	}
	next := now.Add(time.Duration(seconds) * time.Second)
	if previous != nil && previous.After(next) {
		next = *previous
	}
	return &next
}

func legacyClipboardRetention(created time.Time, expires *time.Time) int {
	if expires == nil {
		return 0
	}
	duration := expires.Sub(created)
	for _, seconds := range []int{1800, 14400, 86400, 604800, 2592000} {
		if duration <= time.Duration(seconds)*time.Second {
			return seconds
		}
	}
	// A legacy deadline is preserved even if it exceeds the selectable policy.
	return 2592000
}

func laterClipboardTime(now, previous time.Time) time.Time {
	if !now.After(previous) {
		return previous.Add(time.Nanosecond)
	}
	return now
}
