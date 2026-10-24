//go:build linux

package claudecode

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
	"time"
)

// killMarked SIGKILLs every process whose environment carries the session
// marker, rescanning until none is left or bound passes. It catches what the
// process-group kill misses: the CLI starts Bash tool shells, hooks and its
// pty host detached (setsid), outside its process group.
//
// Limits: a process that scrubs its environment, or one owned by another
// user (unreadable environ), escapes. Full containment (cgroups) belongs to
// the host program.
func killMarked(marker string, bound time.Duration) int {
	if marker == "" {
		return 0
	}
	needle := []byte(SessionMarkerEnv + "=" + marker)
	self := os.Getpid()
	deadline := time.Now().Add(bound)
	killed := 0
	for {
		found := 0
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return killed
		}
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil || pid == self {
				continue
			}
			env, err := os.ReadFile("/proc/" + e.Name() + "/environ")
			if err != nil || !hasEnvEntry(env, needle) {
				continue
			}
			found++
			if syscall.Kill(pid, syscall.SIGKILL) == nil {
				killed++
			}
		}
		if found == 0 || time.Now().After(deadline) {
			return killed
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// hasEnvEntry reports whether the NUL-separated environ block has the exact
// KEY=value entry.
func hasEnvEntry(environ, entry []byte) bool {
	for _, kv := range bytes.Split(environ, []byte{0}) {
		if bytes.Equal(kv, entry) {
			return true
		}
	}
	return false
}
