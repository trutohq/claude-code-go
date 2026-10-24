//go:build unix

package claudecode_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The fake claude is this test binary re-executed with FAKECLAUDE=1. It
// replays a JSON script (FAKECLAUDE_SCRIPT) step by step: emit stdout lines,
// wait for stdin lines and answer control requests, send control requests
// and echo what the driver answered, write stderr, exit.
//
// It logs its argv, selected environment and every stdin line into
// FAKECLAUDE_LOG so tests can check what the driver sent.

func TestMain(m *testing.M) {
	if os.Getenv("FAKECLAUDE") == "1" {
		os.Exit(fakeMain())
	}
	os.Exit(m.Run())
}

type fakeStep struct {
	Op         string          `json:"op"`
	Line       json.RawMessage `json:"line,omitempty"`
	Text       string          `json:"text,omitempty"`
	Type       string          `json:"type,omitempty"`
	Subtype    string          `json:"subtype,omitempty"`
	Reply      json.RawMessage `json:"reply,omitempty"`
	ReplyError string          `json:"reply_error,omitempty"`
	Echo       bool            `json:"echo,omitempty"`
	Request    json.RawMessage `json:"request,omitempty"`
	ID         string          `json:"id,omitempty"`
	Code       int             `json:"code,omitempty"`
	MS         int             `json:"ms,omitempty"`
	IgnoreTerm bool            `json:"ignore_term,omitempty"`
	Setsid     bool            `json:"setsid,omitempty"`
}

type fakeIn struct {
	lines   chan []byte
	backlog [][]byte
	log     *os.File
}

func (f *fakeIn) next() ([]byte, bool) {
	if len(f.backlog) > 0 {
		l := f.backlog[0]
		f.backlog = f.backlog[1:]
		return l, true
	}
	l, ok := <-f.lines
	return l, ok
}

// waitFor returns the first stdin line matching match; lines that do not
// match are kept for later steps.
func (f *fakeIn) waitFor(match func(map[string]any) bool) (map[string]any, []byte, bool) {
	var skipped [][]byte
	defer func() { f.backlog = append(skipped, f.backlog...) }()
	for {
		l, ok := f.next()
		if !ok {
			return nil, nil, false
		}
		var m map[string]any
		if json.Unmarshal(l, &m) == nil && match(m) {
			return m, l, true
		}
		skipped = append(skipped, l)
	}
}

func str(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[p]
	}
	s, _ := cur.(string)
	return s
}

func fakeMain() int {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		v := os.Getenv("FAKECLAUDE_VERSION")
		if v == "" {
			v = "2.1.296"
		}
		fmt.Printf("%s (Claude Code)\n", v)
		return 0
	}
	logDir := os.Getenv("FAKECLAUDE_LOG")
	if logDir != "" {
		b, _ := json.Marshal(args)
		_ = os.WriteFile(filepath.Join(logDir, "args.json"), b, 0o644)
		env := map[string]string{}
		for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CONFIG_DIR", "DISABLE_AUTOUPDATER",
			"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS", "PWD", "EXTRA_VAR", "CLAUDE_CODE_MESSAGING_SOCKET", "HOST_SECRET",
			"TRACEPARENT", "PATH", "HOME", "CCGO_AGENT_SESSION"} {
			if v, ok := os.LookupEnv(k); ok {
				env[k] = v
			}
		}
		wd, _ := os.Getwd()
		env["_cwd"] = wd
		b, _ = json.Marshal(env)
		_ = os.WriteFile(filepath.Join(logDir, "env.json"), b, 0o644)
	}
	var steps []fakeStep
	if err := json.Unmarshal([]byte(os.Getenv("FAKECLAUDE_SCRIPT")), &steps); err != nil {
		fmt.Fprintln(os.Stderr, "fake: bad script:", err)
		return 99
	}
	in := &fakeIn{lines: make(chan []byte, 1024)}
	if logDir != "" {
		in.log, _ = os.Create(filepath.Join(logDir, "stdin.jsonl"))
	}
	var frozen atomic.Bool
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			if frozen.Load() {
				select {} // stop reading: the pipe fills up
			}
			l := append([]byte(nil), sc.Bytes()...)
			if in.log != nil {
				in.log.Write(append(l, '\n'))
				in.log.Sync()
			}
			in.lines <- l
		}
		close(in.lines)
	}()
	out := bufio.NewWriter(os.Stdout)
	emit := func(b []byte) {
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
	emitJSON := func(v any) {
		b, _ := json.Marshal(v)
		emit(b)
	}
	var lastRequestID string
	for _, st := range steps {
		switch st.Op {
		case "emit":
			emit(st.Line)
		case "emit_raw":
			emit([]byte(st.Text))
		case "stderr":
			fmt.Fprintln(os.Stderr, st.Text)
		case "sleep":
			time.Sleep(time.Duration(st.MS) * time.Millisecond)
		case "read":
			m, raw, ok := in.waitFor(func(m map[string]any) bool {
				if str(m, "type") != st.Type {
					return false
				}
				if st.Subtype == "" {
					return true
				}
				return str(m, "request", "subtype") == st.Subtype || str(m, "response", "subtype") == st.Subtype
			})
			if !ok {
				fmt.Fprintf(os.Stderr, "fake: stdin closed while waiting for %s/%s\n", st.Type, st.Subtype)
				return 98
			}
			if st.Echo {
				emitJSON(map[string]any{"type": "fake_read", "line": json.RawMessage(raw)})
			}
			lastRequestID = str(m, "request_id")
			if st.Type == "control_request" && (st.Reply != nil || st.ReplyError != "") {
				id := lastRequestID
				if st.ReplyError != "" {
					emitJSON(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": st.ReplyError}})
				} else {
					emitJSON(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": st.Reply}})
				}
			}
		case "reply_last":
			emitJSON(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": lastRequestID, "response": st.Reply}})
		case "spawn_sleeper":
			// A background command that inherits stdout, like an agent's
			// dev server; it must not keep the session open.
			// It really sleeps (a "block" would die at once: with stdin at
			// EOF the Go runtime reports a deadlock). It does not log.
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), "FAKECLAUDE=1", `FAKECLAUDE_SCRIPT=[{"op":"sleep","ms":60000}]`, "FAKECLAUDE_LOG=")
			cmd.Stdout = os.Stdout
			if st.Setsid {
				// Like the CLI's Bash tool shells: outside the process group.
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			}
			if err := cmd.Start(); err != nil {
				fmt.Fprintln(os.Stderr, "fake: spawn:", err)
				return 95
			}
			emitJSON(map[string]any{"type": "fake_sleeper", "pid": cmd.Process.Pid})
		case "request":
			emitJSON(map[string]any{"type": "control_request", "request_id": st.ID, "request": st.Request})
			m, _, ok := in.waitFor(func(m map[string]any) bool {
				return str(m, "type") == "control_response" && str(m, "response", "request_id") == st.ID
			})
			if !ok {
				fmt.Fprintf(os.Stderr, "fake: stdin closed while waiting for response to %s\n", st.ID)
				return 97
			}
			emitJSON(map[string]any{"type": "fake_echo", "id": st.ID, "response": m["response"]})
		case "freeze_stdin":
			frozen.Store(true)
		case "cancel":
			emitJSON(map[string]any{"type": "control_cancel_request", "request_id": st.ID})
		case "wait_eof":
			for {
				if _, ok := in.next(); !ok {
					break
				}
			}
		case "exit":
			out.Flush()
			return st.Code
		case "block":
			if st.IgnoreTerm {
				signal.Ignore(syscall.SIGTERM)
			}
			// Said once SIGTERM is ignored, so a test can wait for it.
			emitJSON(map[string]any{"type": "fake_blocking"})
			// Not select {}: with every goroutine blocked the Go runtime
			// reports a deadlock and exits with code 2 (the race
			// detector's runtime hides that). A sleeping goroutine keeps
			// the process alive until it is killed.
			for {
				time.Sleep(time.Hour)
			}
		default:
			fmt.Fprintln(os.Stderr, "fake: unknown op", st.Op)
			return 96
		}
	}
	// Like the CLI: run until input ends.
	for {
		if _, ok := in.next(); !ok {
			return 0
		}
	}
}

// readStdinLog returns the stdin lines the fake received.
func readStdinLog(t *testing.T, dir string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "stdin.jsonl"))
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdin log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}
