package proto

import (
	"fmt"
	"testing"
)

func TestAdoptHistoryWireBounds(t *testing.T) {
	for _, rows := range []int{-1, 0, 500, 5000, 10000, 10001} {
		t.Run(fmt.Sprint(rows), func(t *testing.T) {
			control, err := DecodeClientControl([]byte(fmt.Sprintf(`{"type":"adopt","server_label":"private","session_id":"$1","history_rows":%d}`, rows)))
			valid := rows >= 0 && rows <= 10000
			if (err == nil) != valid {
				t.Fatalf("depth=%d error=%v", rows, err)
			}
			if valid && (control.HistoryRows == nil || *control.HistoryRows != rows) {
				t.Fatal("wire lost the history selection")
			}
		})
	}
}
