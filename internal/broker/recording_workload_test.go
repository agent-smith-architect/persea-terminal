package broker

import (
	"encoding/json"
	"os"
	"testing"
)

type recordingWorkloadSegment struct {
	StartNS int64 `json:"start_ns"`
	EndNS   int64 `json:"end_ns"`
	Rate    int64 `json:"events_per_second_per_source"`
	PhaseNS int64 `json:"source_phase_ns"`
}

func TestRecordingCorrectionBurstWorkloadSchedule(t *testing.T) {
	var manifest struct {
		Execution struct {
			DurationNS int64 `json:"quick_duration_ns"`
		} `json:"execution"`
		Lanes []struct {
			ID           string                     `json:"id"`
			Sources      int                        `json:"sources"`
			Rate         int64                      `json:"events_per_second_per_source"`
			PhaseNS      int64                      `json:"source_phase_ns"`
			BurstNS      int64                      `json:"burst_duration_ns"`
			AfterRate    int64                      `json:"after_burst_events_per_second_per_source"`
			AfterPhaseNS int64                      `json:"after_burst_source_phase_ns"`
			Segments     []recordingWorkloadSegment `json:"segments"`
			PayloadIndex string                     `json:"payload_event_index"`
		} `json:"lanes"`
	}
	data, err := os.ReadFile("testdata/recording-workload-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, lane := range manifest.Lanes {
		if lane.ID != "bounded_burst" {
			continue
		}
		segments := lane.Segments
		want := []recordingWorkloadSegment{
			{0, lane.BurstNS, lane.Rate, lane.PhaseNS},
			{lane.BurstNS, manifest.Execution.DurationNS, lane.AfterRate, lane.AfterPhaseNS},
		}
		if len(segments) != 2 {
			t.Error("burst schedule must declare exactly two contiguous segments")
		} else {
			for index := range want {
				if segments[index] != want[index] {
					t.Errorf("segment %d differs from declared lane transition: %+v want %+v", index, segments[index], want[index])
				}
			}
		}
		// Before the piecewise contract, the published formula applies its
		// constant rate for the whole lane. This reproduces that ambiguity.
		if len(segments) == 0 {
			segments = []recordingWorkloadSegment{{0, manifest.Execution.DurationNS, lane.Rate, lane.PhaseNS}}
		}
		counts := [2]int{}
		for source := 0; source < lane.Sources; source++ {
			payloadIndex := 0
			last := int64(-1)
			firstAfter := int64(-1)
			for _, segment := range segments {
				if segment.Rate <= 0 || 1000000000%segment.Rate != 0 {
					t.Fatal("non-integral schedule period")
				}
				period := 1000000000 / segment.Rate
				for at := segment.StartNS + int64(source)*segment.PhaseNS; at < segment.EndNS; at += period {
					if at <= last {
						t.Fatal("non-monotonic or duplicated boundary event")
					}
					last = at
					part := 0
					if at >= 1000000000 {
						part = 1
						if firstAfter < 0 {
							firstAfter = at
							if payloadIndex != 400 {
								t.Errorf("source %d payload index at transition=%d want=400", source, payloadIndex)
							}
						}
					}
					counts[part]++
					payloadIndex++
				}
			}
			if firstAfter != 1000000000+int64(source)*3125000 {
				t.Errorf("source %d first post-burst event=%d", source, firstAfter)
			}
			if payloadIndex != 2760 {
				t.Errorf("source %d payload events=%d want=2760", source, payloadIndex)
			}
		}
		t.Logf("burst events before/after transition=%d/%d total=%d", counts[0], counts[1], counts[0]+counts[1])
		if counts != [2]int{3200, 18880} {
			t.Errorf("piecewise workload counts=%v total=%d want=[3200 18880] total=22080", counts, counts[0]+counts[1])
		}
		if lane.PayloadIndex != "continuous_per_source_across_segments" {
			t.Error("payload event index continuity is not explicit")
		}
		return
	}
	t.Fatal("bounded burst lane missing")
}
