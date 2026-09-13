//go:build race

package broker

// raceDetectorEnabled reports whether this test binary carries the race
// detector; pins whose only verdict is "no data race" skip without it.
const raceDetectorEnabled = true
