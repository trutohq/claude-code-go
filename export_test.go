package claudecode

import "time"

// SetStderrReadDelay holds the stderr reader back by d before its first
// read, until the test ends: a busy computer, on demand.
func SetStderrReadDelay(t interface{ Cleanup(func()) }, d time.Duration) {
	stderrReadDelay = d
	t.Cleanup(func() { stderrReadDelay = 0 })
}
