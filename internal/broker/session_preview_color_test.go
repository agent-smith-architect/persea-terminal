package broker

import (
	"strings"
	"testing"
)

func TestPreviewANSIOnlyRetainsBoundedSGR(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"\x1b[31mred\x1b[0m", "\x1b[31mred\x1b[0m"},
		{"\x1b[38;2;2;40;255mrgb", "\x1b[38;2;2;40;255mrgb"},
		{"\x1b]52;c;c2VjcmV0\ahello", "hello"},
		{"\x1b]8;;https://secret\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"before\x1b[2Jafter\x1b[1;2H", "beforeafter"},
		{"a\x00\x07\r\x7fb\t世界\n", "ab\t世界\n"},
		{"a\x1bPprivate\x1b\\b", "ab"},
		{"a\x1b]unterminated", "a"},
		{"a\x1b[" + strings.Repeat("1;", 100) + "31mb", "ab"},
		{"a\u0085b", "ab"},
	} {
		if got := sanitizePreviewANSI(test.input); got != test.want {
			t.Fatalf("sanitizer: %q != %q", got, test.want)
		}
	}
}

func TestPreviewDroppedRowsKeepColorAndResets(t *testing.T) {
	input := "\x1b[1;38;2;20;40;60mfirst\nsecond\n\x1b[22;39;48;5;200mthird\nfourth\n"
	rows := strings.Split(previewRowsWithStyle(input), "\n")
	if !strings.HasPrefix(rows[1], "\x1b[0m\x1b[1;38;2;20;40;60m") {
		t.Fatalf("inherited color lost: %q", rows[1])
	}
	if !strings.HasPrefix(rows[3], "\x1b[0m\x1b[48;5;200m") {
		t.Fatalf("reset did not clear old style: %q", rows[3])
	}
	data, truncated := boundHistory([]byte(previewRowsWithStyle(input)), 1, 4096)
	if !truncated || !strings.Contains(string(data), "\x1b[48;5;200mfourth") {
		t.Fatalf("bounded row lost its color: %q", data)
	}
}
