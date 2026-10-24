//go:build !linux

package claudecode

import "time"

// killMarked needs /proc; elsewhere only the process-group kill applies.
func killMarked(string, time.Duration) int { return 0 }
