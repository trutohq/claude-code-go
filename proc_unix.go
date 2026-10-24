//go:build unix

package claudecode

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcAttrs puts the CLI in its own process group, so it and everything it
// starts (tool commands, MCP servers) can be signalled together, and a ^C
// meant for the host program does not reach it.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateGroup(p *os.Process) {
	_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
}

func killGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func exitSignal(ps *os.ProcessState) string {
	if ps == nil {
		return ""
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
