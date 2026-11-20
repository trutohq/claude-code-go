package agent

import (
	"strings"
	"testing"
)

func TestExitErrorReason(t *testing.T) {
	node := strings.Join([]string{
		"/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js:1234",
		`    throw new Error("boom");`,
		"    ^",
		"",
		"Error: ENOSPC: no space left on device, write",
		"    at Object.writeSync (node:fs:927:3)",
		"    at Module._compile (node:internal/modules/cjs/loader:1256:14)",
		"    at node:internal/main/run_main_module:28:49 {",
		"  errno: -28,",
		"  code: 'ENOSPC'",
		"}",
		"",
		"Node.js v22.3.0",
	}, "\n")
	goPanic := strings.Join([]string{
		"panic: runtime error: index out of range [3] with length 2",
		"",
		"goroutine 1 [running]:",
		"main.main()",
		"\t/tmp/x/main.go:5 +0x1d",
		"exit status 2",
	}, "\n")
	cases := []struct {
		name string
		e    ExitError
		want string
	}{
		{"error result wins", ExitError{Code: 1, Result: "Reached maximum number of turns (3)", Stderr: "Error: other"}, "Reached maximum number of turns (3)"},
		{"node uncaught error", ExitError{Code: 1, Stderr: node}, "Error: ENOSPC: no space left on device, write"},
		{"go panic", ExitError{Code: 2, Stderr: goPanic}, "panic: runtime error: index out of range [3] with length 2"},
		{"plain last line", ExitError{Code: 3, Stderr: "starting\nfake: simulated crash\n"}, "fake: simulated crash"},
		{"only a footer", ExitError{Code: 3, Stderr: "Node.js v22.3.0\n"}, "it exited with code 3"},
		{"no stderr", ExitError{Code: 3}, "it exited with code 3"},
		{"signal", ExitError{Code: -1, Signal: "SIGKILL"}, "it was stopped by signal SIGKILL"},
		{"several lines of result", ExitError{Code: 1, Result: "API Error:\n  overloaded"}, "API Error: overloaded"},
	}
	for _, c := range cases {
		if got := c.e.Reason(); got != c.want {
			t.Errorf("%s: Reason() = %q, want %q", c.name, got, c.want)
		}
	}
	long := ExitError{Code: 1, Stderr: "Error: " + strings.Repeat("x", 1000)}
	if r := []rune(long.Reason()); len(r) != reasonMax || r[len(r)-1] != '…' {
		t.Errorf("long reason has %d runes", len(r))
	}
}
