//go:build race

package dispatch

// raceDetector is true when the tests run with the race detector, which slows the workers and
// the recording server, on the same CPUs, by several times.
const raceDetector = true
