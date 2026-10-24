//go:build !unix

package claudecode

import (
	"os"
	"os/exec"
)

// Without process groups only the CLI itself can be stopped.
func setProcAttrs(*exec.Cmd) {}

func terminateGroup(p *os.Process) { _ = p.Kill() }

func killGroup(int) {}

func exitSignal(*os.ProcessState) string { return "" }
