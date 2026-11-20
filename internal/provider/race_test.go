//go:build race

package provider

// raceDetector is true when the tests run with the race detector, which slows the client and
// the recording server, on the same CPUs, by several times.
const raceDetector = true
