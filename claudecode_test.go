//go:build unix

package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	claudecode "github.com/trutohq/claude-code-go"
	"github.com/trutohq/claude-code-go/agent"
)

const testTimeout = 15 * time.Second

type step = map[string]any

func emit(line any) step    { return step{"op": "emit", "line": line} }
func emitRaw(s string) step { return step{"op": "emit_raw", "text": s} }

func initOK() step {
	return step{"op": "read", "type": "control_request", "subtype": "initialize",
		"reply": map[string]any{"claude_code_version": claudecode.TestedVersion, "current_permission_mode": "default"}}
}

func request(id string, req any) step { return step{"op": "request", "id": id, "request": req} }

type harness struct {
	t    *testing.T
	s    *claudecode.Session
	dir  string
	seen []agent.Event
}

// start runs the fake claude with the given script.
func start(t *testing.T, d *claudecode.Driver, steps []step, opts agent.Options, extra claudecode.Extra) (*harness, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return startCtx(ctx, t, d, steps, opts, extra)
}

func startCtx(ctx context.Context, t *testing.T, d *claudecode.Driver, steps []step, opts agent.Options, extra claudecode.Extra) (*harness, error) {
	t.Helper()
	script, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if d == nil {
		d = claudecode.New()
	}
	opts.BinaryPath = os.Args[0]
	if opts.WorkDir == "" {
		opts.WorkDir = t.TempDir()
	}
	// GORACE: a -race binary otherwise sleeps a second before exiting.
	env := map[string]string{"FAKECLAUDE": "1", "FAKECLAUDE_SCRIPT": string(script), "FAKECLAUDE_LOG": dir, "GORACE": "atexit_sleep_ms=0"}
	for k, v := range opts.Env {
		env[k] = v
	}
	opts.Env = env
	if extra.ShutdownGrace == 0 {
		extra.ShutdownGrace = 2 * time.Second
	}
	if extra.InitTimeout == 0 {
		extra.InitTimeout = testTimeout
	}
	s, err := d.StartSession(ctx, opts, extra)
	if err != nil {
		return nil, err
	}
	h := &harness{t: t, s: s, dir: dir}
	t.Cleanup(func() {
		_ = s.Close()
		for range s.Events() {
		}
	})
	return h, nil
}

func mustStart(t *testing.T, steps []step, opts agent.Options) *harness {
	t.Helper()
	h, err := start(t, nil, steps, opts, claudecode.Extra{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return h
}

// until reads events until one matches.
func (h *harness) until(what string, pred func(agent.Event) bool) agent.Event {
	h.t.Helper()
	timer := time.NewTimer(testTimeout)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-h.s.Events():
			if !ok {
				h.t.Fatalf("events closed while waiting for %s", what)
			}
			h.seen = append(h.seen, ev)
			if pred(ev) {
				return ev
			}
		case <-timer.C:
			h.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (h *harness) untilKind(k agent.Kind) agent.Event {
	h.t.Helper()
	return h.until(string(k), func(ev agent.Event) bool { return ev.Kind == k })
}

// echo waits for the fake's report of the driver's reply to request id.
func (h *harness) echo(id string) map[string]any {
	h.t.Helper()
	ev := h.until("echo "+id, func(ev agent.Event) bool {
		if ev.Type != "fake_echo" {
			return false
		}
		var m map[string]any
		_ = json.Unmarshal(ev.Raw, &m)
		return m["id"] == id
	})
	var m struct {
		Response map[string]any `json:"response"`
	}
	if err := json.Unmarshal(ev.Raw, &m); err != nil {
		h.t.Fatal(err)
	}
	return m.Response
}

// drain reads the rest of the events and waits for the process.
func (h *harness) drain() error {
	h.t.Helper()
	timer := time.NewTimer(testTimeout)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-h.s.Events():
			if !ok {
				return h.s.Wait()
			}
			h.seen = append(h.seen, ev)
		case <-timer.C:
			h.t.Fatal("timed out draining events")
		}
	}
}

func (h *harness) args() []string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, "args.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var a []string
	if err := json.Unmarshal(b, &a); err != nil {
		h.t.Fatal(err)
	}
	return a
}

func (h *harness) env() map[string]string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, "env.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var e map[string]string
	if err := json.Unmarshal(b, &e); err != nil {
		h.t.Fatal(err)
	}
	return e
}

func jsonEq(t *testing.T, got any, want string) {
	t.Helper()
	gb, _ := json.Marshal(got)
	var g, w any
	_ = json.Unmarshal(gb, &g)
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("got %s\nwant %s", gb, want)
	}
}

func TestSessionFlow(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent-session")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/tmp/parent.sock")
	t.Setenv("TRACEPARENT", "00-abc-def-01")
	t.Setenv("HOST_SECRET", "s3cret")
	work := t.TempDir()
	const sid = "3f0b8f9e-6a51-4c43-9d55-2f5a8d0e1c7a"
	assistant := `{"type":"assistant","message":{"id":"msg_1","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"Writing it."},{"type":"tool_use","id":"tu_1","name":"Write","input":{"file_path":"a.txt","content":"hi"}}]},"parent_tool_use_id":null,"session_id":"` + sid + `"}`
	h := mustStart(t, []step{
		initOK(),
		{"op": "read", "type": "user"},
		emit(map[string]any{"type": "system", "subtype": "init", "cwd": work, "session_id": sid, "model": "claude-haiku-5-5",
			"permissionMode": "default", "claude_code_version": "2.1.296", "tools": []string{"Read", "Write", "AskUserQuestion"},
			"mcp_servers": []any{map[string]any{"name": "tools", "status": "connected"}}}),
		emit(map[string]any{"type": "system", "subtype": "session_state_changed", "state": "running", "session_id": sid}),
		emitRaw(assistant),
		emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": "File created", "is_error": false}}},
			"parent_tool_use_id": nil, "session_id": sid}),
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Done.", "num_turns": 2,
			"duration_ms": 1234, "total_cost_usd": 0.0062, "stop_reason": "end_turn", "terminal_reason": "completed", "session_id": sid}),
		emit(map[string]any{"type": "system", "subtype": "session_state_changed", "state": "idle", "session_id": sid}),
	}, agent.Options{
		WorkDir: work, SessionID: sid, Model: "haiku", Effort: "low", MaxTurns: 3,
		AllowedTools: []string{"Read", "Write"}, ConfigDir: "/tmp/cfg-x",
		AppendSystemPrompt: "-be brief", Env: map[string]string{"EXTRA_VAR": "yes"},
	})
	if got := h.s.ClaudeInfo().PermissionMode; got != "default" {
		t.Errorf("ClaudeInfo().PermissionMode = %q", got)
	}
	if err := h.s.Send(context.Background(), agent.UserMessage{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	h.untilKind(agent.KindResult)
	h.until("idle", func(ev agent.Event) bool { return ev.Kind == agent.KindSessionState && ev.State == "idle" })
	if err := h.s.CloseInput(); err != nil {
		t.Fatal(err)
	}
	if err := h.drain(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := h.s.Send(context.Background(), agent.UserMessage{Text: "late"}); !errors.Is(err, agent.ErrClosed) {
		t.Errorf("Send after close: %v, want ErrClosed", err)
	}

	var kinds []agent.Kind
	var byKind = map[agent.Kind]agent.Event{}
	for _, ev := range h.seen {
		if ev.Kind == agent.KindSent || ev.Kind == agent.KindControl {
			continue
		}
		kinds = append(kinds, ev.Kind)
		if _, ok := byKind[ev.Kind]; !ok {
			byKind[ev.Kind] = ev
		}
	}
	want := []agent.Kind{agent.KindInit, agent.KindSessionState, agent.KindAssistant, agent.KindUser, agent.KindResult, agent.KindSessionState}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	in := byKind[agent.KindInit].Init
	if in == nil || in.Model != "claude-haiku-5-5" || in.PermissionMode != "default" || in.Version != "2.1.296" ||
		!slices.Contains(in.Tools, "AskUserQuestion") || len(in.MCPServers) != 1 || in.MCPServers[0].Status != "connected" {
		t.Errorf("init = %+v", in)
	}
	a := byKind[agent.KindAssistant]
	if string(a.Raw) != assistant {
		t.Errorf("assistant raw = %s", a.Raw)
	}
	if a.Message == nil || a.Message.Text() != "Writing it." || len(a.Message.Content) != 2 ||
		a.Message.Content[1].Name != "Write" || a.Message.Content[1].ID != "tu_1" || a.SessionID != sid {
		t.Errorf("assistant = %+v", a.Message)
	}
	u := byKind[agent.KindUser]
	if u.Message == nil || u.Message.Content[0].Type != "tool_result" || u.Message.Content[0].ToolUseID != "tu_1" {
		t.Errorf("user = %+v", u.Message)
	}
	r := byKind[agent.KindResult].Result
	if r == nil || r.Subtype != "success" || r.Text != "Done." || r.NumTurns != 2 || r.DurationMS != 1234 ||
		r.TotalCostUSD != 0.0062 || r.TerminalReason != "completed" {
		t.Errorf("result = %+v", r)
	}
	if h.s.SessionID() != sid {
		t.Errorf("SessionID = %q", h.s.SessionID())
	}

	// What was written, in order, is in the event stream too.
	var sent []string
	for _, ev := range h.seen {
		if ev.Kind == agent.KindSent {
			sent = append(sent, ev.Type+"/"+ev.Subtype)
		}
	}
	if !slices.Equal(sent, []string{"control_request/initialize", "user/"}) {
		t.Errorf("sent = %v", sent)
	}
	stdin := readStdinLog(t, h.dir)
	if len(stdin) != 2 {
		t.Fatalf("stdin = %v", stdin)
	}
	jsonEq(t, stdin[0]["request"], `{"subtype":"initialize","hooks":null}`)
	jsonEq(t, stdin[1], `{"type":"user","session_id":"","message":{"role":"user","content":"hello"},"parent_tool_use_id":null}`)

	args := h.args()
	for _, want := range []string{"-p", "--input-format=stream-json", "--output-format=stream-json", "--verbose",
		"--session-id=" + sid, "--model=haiku", "--effort=low", "--max-turns=3", "--allowedTools=Read,Write",
		"--permission-mode=default", "--permission-prompts=none", "--append-system-prompt=-be brief", "--setting-sources="} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v missing %q", args, want)
		}
	}
	env := h.env()
	for k, v := range map[string]string{"CLAUDE_CODE_ENTRYPOINT": "sdk-cli", "DISABLE_AUTOUPDATER": "1",
		"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS": "1", "CLAUDE_CONFIG_DIR": "/tmp/cfg-x", "EXTRA_VAR": "yes", "PWD": work} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_MESSAGING_SOCKET", "TRACEPARENT", "HOST_SECRET"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s reached the agent", k)
		}
	}
	if env["PATH"] == "" || env["HOME"] == "" || len(env["CCGO_AGENT_SESSION"]) != 32 {
		t.Errorf("allowlisted or marker variables missing: %v", env)
	}
	info := h.s.Info()
	if info.Agent != "claude-code" || info.Model != "claude-haiku-5-5" || info.Version != claudecode.TestedVersion || info.PID == 0 {
		t.Errorf("Info() = %+v", info)
	}
	if real, _ := filepath.EvalSymlinks(work); env["_cwd"] != work && env["_cwd"] != real {
		t.Errorf("cwd = %q, want %q", env["_cwd"], work)
	}
}

func TestPermissionAllowDenyUpdatedInput(t *testing.T) {
	reqs := make(chan agent.PermissionRequest, 4)
	perm := func(ctx context.Context, r agent.PermissionRequest) (agent.PermissionDecision, error) {
		reqs <- r
		switch r.ToolUseID {
		case "t1":
			return agent.Allow(), nil
		case "t2":
			return agent.PermissionDecision{Allow: true, UpdatedInput: json.RawMessage(`{"file_path":"/safe/a.txt","content":"hi"}`),
				UpdatedPermissions: json.RawMessage(`[{"type":"setMode","mode":"acceptEdits","destination":"session"}]`)}, nil
		case "t3":
			return agent.PermissionDecision{Message: "not in this project", Interrupt: true}, nil
		default:
			return agent.PermissionDecision{}, errors.New("boom")
		}
	}
	canUse := func(id string) map[string]any {
		return map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "display_name": "Write", "description": "a.txt",
			"input": map[string]any{"file_path": "/w/a.txt", "content": "hi"}, "tool_use_id": id,
			"permission_suggestions": []any{map[string]any{"type": "setMode", "mode": "acceptEdits", "destination": "session"}}}
	}
	h := mustStart(t, []step{
		initOK(),
		request("p1", canUse("t1")),
		request("p2", canUse("t2")),
		request("p3", canUse("t3")),
		request("p4", canUse("t4")),
	}, agent.Options{Permission: perm})

	jsonEq(t, h.echo("p1"), `{"subtype":"success","request_id":"p1","response":{"behavior":"allow","updatedInput":{"file_path":"/w/a.txt","content":"hi"}}}`)
	jsonEq(t, h.echo("p2"), `{"subtype":"success","request_id":"p2","response":{"behavior":"allow","updatedInput":{"file_path":"/safe/a.txt","content":"hi"},
		"updatedPermissions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}]}}`)
	jsonEq(t, h.echo("p3"), `{"subtype":"success","request_id":"p3","response":{"behavior":"deny","message":"not in this project","interrupt":true}}`)
	jsonEq(t, h.echo("p4"), `{"subtype":"error","request_id":"p4","error":"boom"}`)

	r := <-reqs
	if r.ToolName != "Write" || r.DisplayName != "Write" || r.Description != "a.txt" || r.IsQuestion() || len(r.Raw) == 0 {
		t.Errorf("request = %+v", r)
	}
	jsonEq(t, r.Input, `{"file_path":"/w/a.txt","content":"hi"}`)
	if args := h.args(); !slices.Contains(args, "--permission-prompt-tool=stdio") || slices.Contains(args, "--permission-prompts=none") {
		t.Errorf("args = %v", args)
	}
	_ = h.s.CloseInput()
	if err := h.drain(); err != nil {
		t.Fatal(err)
	}
}

func TestAskUserQuestionAnsweredLater(t *testing.T) {
	asked := make(chan agent.PermissionRequest, 1)
	answer := make(chan map[string]string)
	perm := func(ctx context.Context, r agent.PermissionRequest) (agent.PermissionDecision, error) {
		asked <- r
		select {
		case a := <-answer: // a person answers, some time later
			return agent.Answer(a), nil
		case <-ctx.Done():
			return agent.PermissionDecision{}, ctx.Err()
		}
	}
	questions := []any{map[string]any{"question": "Which color do you prefer?", "header": "Color", "multiSelect": false,
		"options": []any{map[string]any{"label": "Red", "description": "Choose red"}, map[string]any{"label": "Blue", "description": "Choose blue"}}}}
	h := mustStart(t, []step{
		initOK(),
		request("q1", map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "display_name": "AskUserQuestion",
			"input": map[string]any{"questions": questions}, "tool_use_id": "tu_q", "requires_user_interaction": true}),
	}, agent.Options{Permission: perm})

	var r agent.PermissionRequest
	select {
	case r = <-asked:
	case <-time.After(testTimeout):
		t.Fatal("question never asked")
	}
	if !r.IsQuestion() || r.Questions[0].Question != "Which color do you prefer?" || r.Questions[0].Header != "Color" ||
		len(r.Questions[0].Options) != 2 || r.Questions[0].Options[1].Label != "Blue" {
		t.Fatalf("questions = %+v", r.Questions)
	}
	// The session stays usable while the question is open.
	if err := h.s.Send(context.Background(), agent.UserMessage{Text: "still there?"}); err != nil {
		t.Fatal(err)
	}
	answer <- map[string]string{"Which color do you prefer?": "Blue"}
	resp := h.echo("q1")
	want := map[string]any{"behavior": "allow", "updatedInput": map[string]any{"questions": questions,
		"answers": map[string]any{"Which color do you prefer?": "Blue"}}}
	wb, _ := json.Marshal(want)
	jsonEq(t, resp["response"], string(wb))
	_ = h.s.CloseInput()
	if err := h.drain(); err != nil {
		t.Fatal(err)
	}
}

func TestSendMidRunWithPriority(t *testing.T) {
	h := mustStart(t, []step{
		initOK(),
		{"op": "read", "type": "user"},
		emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "working"}}}}),
		{"op": "read", "type": "user", "echo": true},
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "ok"}),
	}, agent.Options{})
	ctx := context.Background()
	if err := h.s.Send(ctx, agent.UserMessage{Text: "build the page"}); err != nil {
		t.Fatal(err)
	}
	h.untilKind(agent.KindAssistant)
	if err := h.s.Send(ctx, agent.UserMessage{Priority: "soon", Text: "x"}); err == nil {
		t.Error("unknown priority accepted")
	}
	if err := h.s.Send(ctx, agent.UserMessage{Text: "  "}); err == nil {
		t.Error("empty message accepted")
	}
	if err := h.s.Send(ctx, agent.UserMessage{Content: json.RawMessage(`{"type":"text"}`)}); err == nil {
		t.Error("non-array content accepted")
	}
	content := json.RawMessage(`[{"type":"text","text":"Make the button blue instead."}]`)
	if err := h.s.Send(ctx, agent.UserMessage{Content: content, Priority: agent.PriorityNow}); err != nil {
		t.Fatal(err)
	}
	ev := h.until("fake_read", func(ev agent.Event) bool { return ev.Type == "fake_read" })
	var m struct {
		Line map[string]any `json:"line"`
	}
	_ = json.Unmarshal(ev.Raw, &m)
	jsonEq(t, m.Line, `{"type":"user","session_id":"","parent_tool_use_id":null,"priority":"now",
		"message":{"role":"user","content":[{"type":"text","text":"Make the button blue instead."}]}}`)
	h.untilKind(agent.KindResult)
	if n := len(readStdinLog(t, h.dir)); n != 3 {
		t.Errorf("stdin has %d lines, want 3 (invalid messages must not be written)", n)
	}
	_ = h.s.CloseInput()
	if err := h.drain(); err != nil {
		t.Fatal(err)
	}
}

func TestInterrupt(t *testing.T) {
	h := mustStart(t, []step{
		initOK(),
		{"op": "read", "type": "control_request", "subtype": "interrupt", "reply": map[string]any{"still_queued": []any{}}},
		emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}}),
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []any{"interrupted"}}),
		{"op": "read", "type": "control_request", "subtype": "interrupt", "reply_error": "nothing to interrupt"},
	}, agent.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h.s.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	r := h.untilKind(agent.KindResult)
	if r.Result == nil || !r.Result.IsError || r.Result.Errors[0] != "interrupted" {
		t.Errorf("result = %+v", r.Result)
	}
	err := h.s.Interrupt(ctx)
	var ce *claudecode.ControlError
	if !errors.As(err, &ce) || ce.Subtype != "interrupt" || ce.Message != "nothing to interrupt" {
		t.Errorf("second Interrupt = %v", err)
	}
	_ = h.s.CloseInput()
	if err := h.drain(); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimitEvent(t *testing.T) {
	h := mustStart(t, []step{
		initOK(),
		// As observed from CLI 2.1.296.
		emitRaw(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792882800,"rateLimitType":"five_hour","utilization":1,"overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"surpassedThreshold":0.75,"unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":1792882800}}},"uuid":"u1","session_id":"s1"}`),
		emitRaw(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"seven_day"},"session_id":"s1"}`),
		emitRaw(`{"type":"rate_limit_event","session_id":"s1"}`),
		emit(map[string]any{"type": "result", "subtype": "success"}),
	}, agent.Options{})
	var rls []agent.Event
	h.untilKind(agent.KindResult)
	for _, ev := range h.seen {
		if ev.Kind == agent.KindRateLimit {
			rls = append(rls, ev)
		}
	}
	if len(rls) != 3 {
		t.Fatalf("got %d rate limit events", len(rls))
	}
	rl := rls[0].RateLimit
	if rl == nil || rl.Status != agent.RateLimitRejected || rl.Type != "five_hour" || rl.Utilization == nil || *rl.Utilization != 1 ||
		!rl.ResetsAt.Equal(time.Unix(1792882800, 0)) || rl.OverageStatus != "rejected" {
		t.Errorf("rate limit = %+v", rl)
	}
	if rl := rls[1].RateLimit; rl == nil || rl.Status != agent.RateLimitAllowedWarning || !rl.ResetsAt.IsZero() || rl.Utilization != nil {
		t.Errorf("partial rate limit = %+v", rl)
	}
	if rls[2].RateLimit != nil || rls[2].ParseError == nil {
		t.Errorf("malformed rate limit: %+v", rls[2])
	}
	_ = h.s.CloseInput()
	if err := h.drain(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessCrash(t *testing.T) {
	h := mustStart(t, []step{
		initOK(),
		{"op": "read", "type": "user"},
		emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{}}}),
		{"op": "stderr", "text": "Error: something broke"},
		{"op": "sleep", "ms": 100},
		{"op": "exit", "code": 3},
	}, agent.Options{})
	if err := h.s.Send(context.Background(), agent.UserMessage{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	h.untilKind(agent.KindAssistant)
	err := h.drain()
	var ee *agent.ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || !strings.Contains(ee.Stderr, "something broke") {
		t.Fatalf("Wait = %#v", err)
	}
	if !slices.ContainsFunc(h.seen, func(ev agent.Event) bool {
		return ev.Kind == agent.KindStderr && string(ev.Raw) == "Error: something broke"
	}) {
		t.Error("stderr line not in events")
	}
	if err := h.s.Interrupt(context.Background()); !errors.Is(err, agent.ErrClosed) {
		t.Errorf("Interrupt after exit = %v, want ErrClosed", err)
	}
}

func TestErrorResultExit(t *testing.T) {
	h := mustStart(t, []step{
		initOK(),
		emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true, "errors": []any{"Reached maximum number of turns (3)"}}),
		{"op": "exit", "code": 1},
	}, agent.Options{})
	err := h.drain()
	var ee *agent.ExitError
	if !errors.As(err, &ee) || ee.Code != 1 || ee.Result != "Reached maximum number of turns (3)" {
		t.Fatalf("Wait = %#v", err)
	}
	if !strings.Contains(err.Error(), "maximum number of turns") {
		t.Errorf("error text = %q", err)
	}
}

func TestPendingRequestFailsOnCrash(t *testing.T) {
	h := mustStart(t, []step{
		initOK(),
		{"op": "read", "type": "control_request", "subtype": "interrupt"}, // never answered
		{"op": "exit", "code": 2},
	}, agent.Options{})
	err := h.s.Interrupt(context.Background())
	if !errors.Is(err, agent.ErrClosed) {
		t.Errorf("Interrupt = %v, want ErrClosed", err)
	}
	var ee *agent.ExitError
	if err := h.drain(); !errors.As(err, &ee) || ee.Code != 2 {
		t.Errorf("Wait = %v", err)
	}
}

func TestUnknownAndRawPassthrough(t *testing.T) {
	lines := []string{
		`{"type":"brand_new_thing","x":1}`,
		`not json at all`,
		`{"type": "system",   "subtype": "post_turn_summary", "status_category": "completed"}`,
		`{"type":"assistant","message":{"content":42}}`,
		`{"type":"user","message":{"role":"user","content":"plain text turn"}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta"}}`,
	}
	steps := []step{initOK()}
	for _, l := range lines {
		steps = append(steps, emitRaw(l))
	}
	steps = append(steps, emitRaw(""), emitRaw("   "), emit(map[string]any{"type": "result", "subtype": "success"}))
	h := mustStart(t, steps, agent.Options{})
	h.untilKind(agent.KindResult)
	var got []agent.Event
	for _, ev := range h.seen {
		if ev.Kind != agent.KindSent && ev.Kind != agent.KindControl && ev.Kind != agent.KindResult {
			got = append(got, ev)
		}
	}
	if len(got) != len(lines) {
		t.Fatalf("got %d events, want %d (blank lines are skipped): %+v", len(got), len(lines), got)
	}
	wantKinds := []agent.Kind{agent.KindUnknown, agent.KindUnknown, agent.KindSystem, agent.KindAssistant, agent.KindUser, agent.KindStreamDelta}
	for i, ev := range got {
		if string(ev.Raw) != lines[i] {
			t.Errorf("event %d raw = %q, want %q", i, ev.Raw, lines[i])
		}
		if ev.Kind != wantKinds[i] {
			t.Errorf("event %d kind = %s, want %s", i, ev.Kind, wantKinds[i])
		}
	}
	if got[0].Type != "brand_new_thing" || got[0].ParseError != nil {
		t.Errorf("unknown type: %+v", got[0])
	}
	if got[1].ParseError == nil {
		t.Error("non-JSON line has no ParseError")
	}
	if got[2].Subtype != "post_turn_summary" {
		t.Errorf("system subtype = %q", got[2].Subtype)
	}
	if got[3].ParseError == nil || got[3].Message != nil {
		t.Errorf("malformed assistant: %+v", got[3])
	}
	if got[4].Message == nil || got[4].Message.Text() != "plain text turn" {
		t.Errorf("string-content user: %+v", got[4].Message)
	}
	_ = h.s.CloseInput()
	if err := h.drain(); err != nil {
		t.Fatal(err)
	}
}

func TestInProcessTools(t *testing.T) {
	calls := make(chan agent.ToolCall, 4)
	srv := agent.ToolServer{Name: "tools", Version: "0.1.0", Tools: []agent.Tool{
		{Name: "echo", Description: "Echo text back", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
			Handler: func(ctx context.Context, c agent.ToolCall) (agent.ToolResult, error) {
				calls <- c
				var a struct{ Text string }
				_ = json.Unmarshal(c.Arguments, &a)
				return agent.TextResult("echoed: " + a.Text), nil
			}},
		{Name: "fail", Handler: func(context.Context, agent.ToolCall) (agent.ToolResult, error) {
			return agent.ToolResult{}, errors.New("record is locked")
		}},
		{Name: "explode", Handler: func(context.Context, agent.ToolCall) (agent.ToolResult, error) { panic("kaboom") }},
	}}
	mcp := func(server string, msg string) map[string]any {
		var m any
		_ = json.Unmarshal([]byte(msg), &m)
		return map[string]any{"subtype": "mcp_message", "server_name": server, "message": m}
	}
	h := mustStart(t, []step{
		// The real CLI initializes SDK servers before it answers initialize.
		{"op": "read", "type": "control_request", "subtype": "initialize"},
		request("m1", mcp("tools", `{"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.296"}},"jsonrpc":"2.0","id":0}`)),
		{"op": "reply_last", "reply": map[string]any{"claude_code_version": claudecode.TestedVersion}},
		request("m2", mcp("tools", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)),
		request("m3", mcp("tools", `{"method":"tools/list","jsonrpc":"2.0","id":1}`)),
		request("m4", mcp("tools", `{"method":"tools/call","params":{"name":"echo","arguments":{"text":"Blue"},"_meta":{"claudecode/toolUseId":"toolu_9","progressToken":2}},"jsonrpc":"2.0","id":2}`)),
		request("m5", mcp("tools", `{"method":"tools/call","params":{"name":"fail","arguments":{}},"jsonrpc":"2.0","id":3}`)),
		request("m6", mcp("tools", `{"method":"tools/call","params":{"name":"explode"},"jsonrpc":"2.0","id":4}`)),
		request("m7", mcp("tools", `{"method":"tools/call","params":{"name":"nope"},"jsonrpc":"2.0","id":5}`)),
		request("m8", mcp("tools", `{"method":"resources/list","jsonrpc":"2.0","id":6}`)),
		request("m9", mcp("other", `{"method":"tools/list","jsonrpc":"2.0","id":7}`)),
		request("m10", map[string]any{"subtype": "mcp_message", "server_name": "tools"}),
		request("m11", map[string]any{"subtype": "rewind_files"}),
		request("m12", mcp("tools", `{"method":"ping","jsonrpc":"2.0","id":"p"}`)),
	}, agent.Options{ToolServers: []agent.ToolServer{srv}, MCPConfig: []string{"/etc/mcp.json"}, StrictMCPConfig: true})

	resp := func(id string) any { return h.echo(id)["response"] }
	jsonEq(t, resp("m1"), `{"mcp_response":{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"tools","version":"0.1.0"}}}}`)
	jsonEq(t, resp("m2"), `{"mcp_response":{"jsonrpc":"2.0","result":{}}}`)
	jsonEq(t, resp("m3"), `{"mcp_response":{"jsonrpc":"2.0","id":1,"result":{"tools":[
		{"name":"echo","description":"Echo text back","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}},
		{"name":"fail","description":"","inputSchema":{"type":"object"}},
		{"name":"explode","description":"","inputSchema":{"type":"object"}}]}}}`)
	jsonEq(t, resp("m4"), `{"mcp_response":{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"echoed: Blue"}]}}}`)
	jsonEq(t, resp("m5"), `{"mcp_response":{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"record is locked"}],"isError":true}}}`)
	jsonEq(t, resp("m6"), `{"mcp_response":{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"tool explode panicked: kaboom"}],"isError":true}}}`)
	jsonEq(t, resp("m7"), `{"mcp_response":{"jsonrpc":"2.0","id":5,"error":{"code":-32602,"message":"Unknown tool: nope"}}}`)
	jsonEq(t, resp("m8"), `{"mcp_response":{"jsonrpc":"2.0","id":6,"error":{"code":-32601,"message":"Method not found: resources/list"}}}`)
	jsonEq(t, resp("m9"), `{"mcp_response":{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"Server 'other' not found"}}}`)
	if e := h.echo("m10"); e["subtype"] != "error" {
		t.Errorf("m10 = %v", e)
	}
	if e := h.echo("m11"); e["subtype"] != "error" || !strings.Contains(e["error"].(string), "unsupported control request subtype: rewind_files") {
		t.Errorf("m11 = %v", e)
	}
	jsonEq(t, resp("m12"), `{"mcp_response":{"jsonrpc":"2.0","id":"p","result":{}}}`)

	c := <-calls
	if c.Server != "tools" || c.Tool != "echo" || c.ToolUseID != "toolu_9" {
		t.Errorf("call = %+v", c)
	}
	args := h.args()
	i := slices.Index(args, "--mcp-config=/etc/mcp.json")
	j := slices.Index(args, `--mcp-config={"mcpServers":{"tools":{"name":"tools","type":"sdk"}}}`)
	if i < 0 || j < 0 || !slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("args = %v", args)
	}
	_ = h.s.CloseInput()
	if err := h.drain(); err != nil {
		t.Fatal(err)
	}
}

func TestControlCancelWithdrawsPrompt(t *testing.T) {
	started := make(chan struct{})
	withdrawn := make(chan struct{})
	perm := func(ctx context.Context, r agent.PermissionRequest) (agent.PermissionDecision, error) {
		close(started)
		<-ctx.Done()
		close(withdrawn)
		return agent.Allow(), nil // too late: must not be sent
	}
	h := mustStart(t, []step{
		initOK(),
		emit(map[string]any{"type": "control_request", "request_id": "c1", "request": map[string]any{
			"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ls"}, "tool_use_id": "tu"}}),
		{"op": "read", "type": "user"}, // the test says when to withdraw
		{"op": "cancel", "id": "c1"},
		{"op": "read", "type": "user"},
	}, agent.Options{Permission: perm})
	ctx := context.Background()
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("prompt never reached the callback")
	}
	if err := h.s.Send(ctx, agent.UserMessage{Text: "withdraw it"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-withdrawn:
	case <-time.After(testTimeout):
		t.Fatal("callback context not cancelled")
	}
	time.Sleep(100 * time.Millisecond) // a wrongly sent reply would land now
	if err := h.s.Send(ctx, agent.UserMessage{Text: "done"}); err != nil {
		t.Fatal(err)
	}
	_ = h.s.CloseInput()
	if err := h.drain(); err != nil {
		t.Fatal(err)
	}
	for _, l := range readStdinLog(t, h.dir) {
		if l["type"] == "control_response" {
			t.Errorf("reply sent for a withdrawn request: %v", l)
		}
	}
}

func TestHookCallbackDefer(t *testing.T) {
	inputs := make(chan claudecode.HookInput, 1)
	hook := func(ctx context.Context, in claudecode.HookInput) (json.RawMessage, error) {
		inputs <- in
		return claudecode.DeferToolUse("waiting for a person"), nil
	}
	initRead := initOK()
	initRead["echo"] = true
	h, err := start(t, nil, []step{
		initRead,
		request("h1", map[string]any{"subtype": "hook_callback", "callback_id": "hook_0", "tool_use_id": "tu_1",
			"input": map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "deploy"}}}),
		request("h2", map[string]any{"subtype": "hook_callback", "callback_id": "hook_9", "input": map[string]any{}}),
		emit(map[string]any{"type": "result", "subtype": "success", "terminal_reason": "tool_deferred",
			"deferred_tool_use": map[string]any{"id": "tu_1", "name": "Bash", "input": map[string]any{"command": "deploy"}}}),
		{"op": "exit", "code": 0},
	}, agent.Options{}, claudecode.Extra{Hooks: map[string][]claudecode.HookMatcher{
		"PreToolUse": {{Matcher: "Bash", Timeout: 30 * time.Second, Callbacks: []claudecode.HookFunc{hook}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ev := h.until("init echo", func(ev agent.Event) bool { return ev.Type == "fake_read" })
	var m struct {
		Line struct {
			Request map[string]any `json:"request"`
		} `json:"line"`
	}
	_ = json.Unmarshal(ev.Raw, &m)
	jsonEq(t, m.Line.Request, `{"subtype":"initialize","hooks":{"PreToolUse":[{"matcher":"Bash","timeout":30,"hookCallbackIds":["hook_0"]}]}}`)
	jsonEq(t, h.echo("h1"), `{"subtype":"success","request_id":"h1","response":{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"defer","permissionDecisionReason":"waiting for a person"}}}`)
	if e := h.echo("h2"); e["subtype"] != "error" {
		t.Errorf("unknown hook id: %v", e)
	}
	in := <-inputs
	if in.ToolUseID != "tu_1" || !strings.Contains(string(in.Input), `"deploy"`) {
		t.Errorf("hook input = %+v", in)
	}
	r := h.untilKind(agent.KindResult)
	if r.Result.Deferred == nil || r.Result.Deferred.ID != "tu_1" || r.Result.Deferred.Name != "Bash" || r.Result.TerminalReason != "tool_deferred" {
		t.Errorf("result = %+v", r.Result)
	}
	if err := h.drain(); err != nil {
		t.Fatal(err)
	}
}

func TestVersionPolicy(t *testing.T) {
	steps := []step{initOK()}
	old := agent.Options{Env: map[string]string{"FAKECLAUDE_VERSION": "2.0.0"}}

	_, err := start(t, claudecode.New(), steps, old, claudecode.Extra{})
	var ve *claudecode.VersionError
	if !errors.As(err, &ve) || ve.Found != "2.0.0" || ve.Tested != claudecode.TestedVersion {
		t.Fatalf("untested --version: %v", err)
	}

	oldInit := []step{{"op": "read", "type": "control_request", "subtype": "initialize",
		"reply": map[string]any{"claude_code_version": "2.0.0"}}}
	h, err := start(t, &claudecode.Driver{AllowUntestedVersion: true}, oldInit, old, claudecode.Extra{})
	if err != nil {
		t.Fatalf("allowed untested version: %v", err)
	}
	if info := h.s.ClaudeInfo(); info.VersionWarning == "" || info.Version != "2.0.0" {
		t.Error("no version warning")
	}
	_ = h.s.Close()

	// The version the running CLI reports in initialize is checked too.
	swapped := []step{{"op": "read", "type": "control_request", "subtype": "initialize",
		"reply": map[string]any{"claude_code_version": "2.1.300"}}}
	_, err = start(t, claudecode.New(), swapped, agent.Options{}, claudecode.Extra{})
	if !errors.As(err, &ve) || ve.Found != "2.1.300" {
		t.Fatalf("initialize version: %v", err)
	}
}

func TestContextCancelKillsStubbornProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := startCtx(ctx, t, nil, []step{initOK(), {"op": "block", "ignore_term": true}}, agent.Options{}, claudecode.Extra{ShutdownGrace: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// Cancel only once the fake ignores SIGTERM.
	h.until("the fake blocking", func(ev agent.Event) bool { return ev.Type == "fake_blocking" })
	t0 := time.Now()
	cancel()
	err = h.drain()
	var ee *agent.ExitError
	if !errors.As(err, &ee) || ee.Signal != syscall.SIGKILL.String() {
		t.Fatalf("Wait = %v", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("shutdown took %v", d)
	}
}

func TestCloseIsGraceful(t *testing.T) {
	h := mustStart(t, []step{initOK()}, agent.Options{})
	if err := h.s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.drain(); err != nil {
		t.Errorf("Wait after Close = %v, want clean exit", err)
	}
}

func TestStartFailures(t *testing.T) {
	t.Run("exits during init", func(t *testing.T) {
		_, err := start(t, nil, []step{{"op": "stderr", "text": "Invalid API key"}, {"op": "exit", "code": 1}}, agent.Options{}, claudecode.Extra{})
		if err == nil || !strings.Contains(err.Error(), "initialize") || !strings.Contains(err.Error(), "Invalid API key") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("init timeout", func(t *testing.T) {
		t0 := time.Now()
		_, err := start(t, nil, []step{{"op": "block"}}, agent.Options{}, claudecode.Extra{InitTimeout: 300 * time.Millisecond})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		if time.Since(t0) > 5*time.Second {
			t.Error("timeout not honoured")
		}
	})
	t.Run("init refused", func(t *testing.T) {
		_, err := start(t, nil, []step{{"op": "read", "type": "control_request", "subtype": "initialize", "reply_error": "bad hooks"}}, agent.Options{}, claudecode.Extra{})
		var ce *claudecode.ControlError
		if !errors.As(err, &ce) || ce.Message != "bad hooks" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad options", func(t *testing.T) {
		for _, o := range []agent.Options{
			{SessionID: "3f0b8f9e-6a51-4c43-9d55-2f5a8d0e1c7a", Resume: "x"},
			{SessionID: "not-a-uuid"},
			{WorkDir: filepath.Join(t.TempDir(), "missing")},
			{ToolServers: []agent.ToolServer{{Name: "bad name"}}},
			{ToolServers: []agent.ToolServer{{Name: "a", Tools: []agent.Tool{{Name: "t"}}}}},
			{MaxTurns: -1},
		} {
			if _, err := start(t, nil, []step{initOK()}, o, claudecode.Extra{}); err == nil {
				t.Errorf("options %+v accepted", o)
			}
		}
	})
	t.Run("no work dir", func(t *testing.T) {
		_, err := claudecode.New().Start(context.Background(), agent.Options{BinaryPath: os.Args[0]})
		if err == nil || !strings.Contains(err.Error(), "WorkDir is required") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		_, err := claudecode.New().Start(context.Background(), agent.Options{WorkDir: t.TempDir(), BinaryPath: filepath.Join(t.TempDir(), "claude")})
		if err == nil || !strings.Contains(err.Error(), "find claude binary") {
			t.Fatalf("err = %v", err)
		}
		if err == nil {
			t.Fatal("started a missing binary")
		}
	})
}

func TestBackgroundChildDoesNotOutliveSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setsid bool
	}{
		{"in the process group", false},
		{"detached with setsid", true}, // how the CLI starts Bash tool shells
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := mustStart(t, []step{initOK(), {"op": "spawn_sleeper", "setsid": tc.setsid}, {"op": "exit", "code": 0}}, agent.Options{})
			ev := h.until("sleeper", func(ev agent.Event) bool { return ev.Type == "fake_sleeper" })
			var m struct{ PID int }
			_ = json.Unmarshal(ev.Raw, &m)
			t.Cleanup(func() { _ = syscall.Kill(m.PID, syscall.SIGKILL) })
			if !alive(m.PID) {
				t.Fatal("sleeper not running before the session ended")
			}
			t0 := time.Now()
			if err := h.drain(); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(t0); d > 8*time.Second {
				t.Errorf("session end took %v", d)
			}
			deadline := time.Now().Add(3 * time.Second)
			for alive(m.PID) {
				if time.Now().After(deadline) {
					t.Fatal("background child survived the session")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

func TestDriverInterface(t *testing.T) {
	var d agent.Driver = claudecode.New()
	if d.Name() != "claude-code" {
		t.Errorf("Name = %q", d.Name())
	}
	script, _ := json.Marshal([]step{initOK()})
	s, err := d.Start(context.Background(), agent.Options{WorkDir: t.TempDir(), BinaryPath: os.Args[0],
		Env: map[string]string{"FAKECLAUDE": "1", "FAKECLAUDE_SCRIPT": string(script), "GORACE": "atexit_sleep_ms=0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range s.Events() {
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
}

// alive reports whether pid is a running (not zombie) process.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // no procfs: trust kill(0)
	}
	// The state follows the parenthesised command name.
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z'
	}
	return true
}

// collectAsync drains a session's events and waits for it, reporting on done.
func collectAsync(s *claudecode.Session) <-chan error {
	done := make(chan error, 1)
	go func() {
		for range s.Events() {
		}
		done <- s.Wait()
	}()
	return done
}

func TestStuckCallbackDoesNotBlockWait(t *testing.T) {
	started := make(chan struct{})
	never := make(chan struct{})
	perm := func(ctx context.Context, r agent.PermissionRequest) (agent.PermissionDecision, error) {
		close(started)
		<-never // waits for a phone answer and ignores ctx
		return agent.Allow(), nil
	}
	h := mustStart(t, []step{
		initOK(),
		emit(map[string]any{"type": "control_request", "request_id": "c1", "request": map[string]any{
			"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ls"}, "tool_use_id": "tu"}}),
		{"op": "sleep", "ms": 200},
		{"op": "exit", "code": 0},
	}, agent.Options{Permission: perm})
	t.Cleanup(func() { close(never) }) // runs before the harness's Close
	<-started
	done := collectAsync(h.s)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait and Events blocked on a callback that ignores its context")
	}
	closed := make(chan struct{})
	go func() { _ = h.s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a callback that ignores its context")
	}
}

func TestFailedStartDoesNotLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	steps := []step{}
	for i := 0; i < 200; i++ { // more than the Events channel buffers
		steps = append(steps, emit(map[string]any{"type": "system", "subtype": "hook_started", "n": i}))
	}
	steps = append(steps, step{"op": "exit", "code": 1})
	if _, err := start(t, nil, steps, agent.Options{}, claudecode.Extra{}); err == nil {
		t.Fatal("start succeeded without an initialize reply")
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after a failed start", before, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDuplicateRequestIDStillCancellable(t *testing.T) {
	cancelled := make(chan struct{})
	perm := func(ctx context.Context, r agent.PermissionRequest) (agent.PermissionDecision, error) {
		if r.ToolUseID == "a" {
			return agent.Allow(), nil
		}
		<-ctx.Done()
		close(cancelled)
		return agent.PermissionDecision{}, ctx.Err()
	}
	canUse := func(tu string) step {
		return emit(map[string]any{"type": "control_request", "request_id": "dup", "request": map[string]any{
			"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{}, "tool_use_id": tu}})
	}
	_ = mustStart(t, []step{
		initOK(),
		canUse("a"),
		canUse("b"), // the CLI reuses the id while "a" is still being served
		{"op": "read", "type": "control_response"},
		{"op": "sleep", "ms": 200}, // let "a"'s handler finish and clean up
		{"op": "cancel", "id": "dup"},
	}, agent.Options{Permission: perm})
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not reach the second request with the reused id")
	}
}

func TestSendHonoursContextOnFullPipe(t *testing.T) {
	h := mustStart(t, []step{initOK(), {"op": "freeze_stdin"}, {"op": "sleep", "ms": 60000}}, agent.Options{})
	big := strings.Repeat("x", 256<<10)
	var sendErr error
	for i := 0; i < 64 && sendErr == nil; i++ {
		res := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			res <- h.s.Send(ctx, agent.UserMessage{Text: big})
		}()
		select {
		case sendErr = <-res:
		case <-time.After(5 * time.Second):
			t.Fatal("Send blocked past its context on a full pipe")
		}
	}
	if !errors.Is(sendErr, context.DeadlineExceeded) {
		t.Fatalf("Send = %v, want DeadlineExceeded", sendErr)
	}
	ev := h.untilKind(agent.KindDriver)
	if ev.Subtype != "write_failed" || ev.Err == nil {
		t.Errorf("driver event = %+v", ev)
	}
}

func TestCallbackFailuresFailClosed(t *testing.T) {
	perm := func(ctx context.Context, r agent.PermissionRequest) (agent.PermissionDecision, error) {
		panic("permission handler bug")
	}
	pre := func(ctx context.Context, in claudecode.HookInput) (json.RawMessage, error) {
		switch in.ToolUseID {
		case "panic":
			panic("hook bug")
		case "err":
			return nil, errors.New("gate unavailable")
		default:
			return json.RawMessage(`[1]`), nil
		}
	}
	post := func(ctx context.Context, in claudecode.HookInput) (json.RawMessage, error) {
		return nil, errors.New("post failed")
	}
	hook := func(id, tu string) step {
		return request(id, map[string]any{"subtype": "hook_callback", "callback_id": "hook_0", "tool_use_id": tu, "input": map[string]any{}})
	}
	// One hook event per session: the driver numbers callbacks while
	// walking its hooks map, so with two events which one gets hook_0
	// varies from run to run.
	h, err := start(t, nil, []step{
		initOK(),
		request("p1", map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{}, "tool_use_id": "x"}),
		hook("pre_1", "panic"),
		hook("pre_2", "err"),
		hook("pre_3", "badjson"),
	}, agent.Options{Permission: perm}, claudecode.Extra{Hooks: map[string][]claudecode.HookMatcher{
		"PreToolUse": {{Callbacks: []claudecode.HookFunc{pre}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if e := h.echo("p1"); e["subtype"] != "error" || !strings.Contains(e["error"].(string), "panicked") {
		t.Errorf("permission panic = %v", e)
	}
	for i, want := range []string{"hook panicked: hook bug", "gate unavailable", "hook output must be a JSON object"} {
		e := h.echo(fmt.Sprintf("pre_%d", i+1))
		resp, _ := e["response"].(map[string]any)
		out, _ := resp["hookSpecificOutput"].(map[string]any)
		if e["subtype"] != "success" || out["permissionDecision"] != "deny" || !strings.Contains(out["permissionDecisionReason"].(string), want) {
			t.Errorf("PreToolUse failure %q = %v", want, e)
		}
	}
	h2, err := start(t, nil, []step{initOK(), hook("post_1", "x")}, agent.Options{}, claudecode.Extra{Hooks: map[string][]claudecode.HookMatcher{
		"PostToolUse": {{Callbacks: []claudecode.HookFunc{post}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if e := h2.echo("post_1"); e["subtype"] != "error" || e["error"] != "post failed" {
		t.Errorf("PostToolUse failure = %v", e)
	}
}

func TestQuestionNeedsAnswers(t *testing.T) {
	got := make(chan agent.PermissionRequest, 1)
	perm := func(ctx context.Context, r agent.PermissionRequest) (agent.PermissionDecision, error) {
		got <- r
		return agent.Allow(), nil // a blanket auto-allow
	}
	q := map[string]any{"question": "How wide?", "header": "Width", "kind": "number", "min": 1, "max": 12, "unit": "cols",
		"options": []any{map[string]any{"label": "4"}, map[string]any{"label": "8"}}}
	h := mustStart(t, []step{
		initOK(),
		request("q1", map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion",
			"input": map[string]any{"questions": []any{q}}, "tool_use_id": "tu"}),
	}, agent.Options{Permission: perm})
	e := h.echo("q1")
	if e["subtype"] != "error" || !strings.Contains(e["error"].(string), "without answers") {
		t.Errorf("unanswered allow = %v", e)
	}
	r := <-got
	if len(r.Questions) != 1 || !strings.Contains(string(r.Questions[0].Raw), `"unit":"cols"`) {
		t.Errorf("question raw = %s", r.Questions[0].Raw)
	}
}

func TestMalformedControlRequests(t *testing.T) {
	h := mustStart(t, []step{
		initOK(),
		emitRaw(`{"type":"control_request","request_id":"m1","request":"oops"}`),
		{"op": "read", "type": "control_response", "subtype": "error", "echo": true},
		emitRaw(`{"type":"control_request","request_id":"m2","request":{"subtype":"can_use_tool","tool_name":7}}`),
		{"op": "read", "type": "control_response", "subtype": "error", "echo": true},
	}, agent.Options{Permission: func(context.Context, agent.PermissionRequest) (agent.PermissionDecision, error) {
		return agent.Allow(), nil
	}})
	for _, id := range []string{"m1", "m2"} {
		ev := h.until("reply "+id, func(ev agent.Event) bool { return ev.Type == "fake_read" })
		var m struct {
			Line struct {
				Response map[string]any `json:"response"`
			} `json:"line"`
		}
		_ = json.Unmarshal(ev.Raw, &m)
		if m.Line.Response["request_id"] != id || m.Line.Response["subtype"] != "error" {
			t.Errorf("reply = %v", m.Line.Response)
		}
	}
}

func TestInheritEnvOption(t *testing.T) {
	t.Setenv("HOST_SECRET", "s3cret")
	t.Setenv("TRACEPARENT", "00-abc-def-01")
	h := mustStart(t, []step{initOK()}, agent.Options{InheritEnv: true})
	env := h.env()
	if env["HOST_SECRET"] != "s3cret" {
		t.Errorf("InheritEnv did not pass the environment: %v", env)
	}
	if _, ok := env["TRACEPARENT"]; ok {
		t.Error("parent-session variable passed with InheritEnv")
	}
}

// A Credential reaches the CLI only through a pipe named by
// *_FILE_DESCRIPTOR: never its environment or its command line, and a
// credential variable of the host's is not passed next to it.
func TestCredentialThroughAPipe(t *testing.T) {
	for _, c := range []struct{ kind, fdVar string }{
		{agent.CredentialOAuthToken, "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR"},
		{agent.CredentialAPIKey, "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR"},
	} {
		t.Run(c.kind, func(t *testing.T) {
			secret := "sk-ant-test-" + c.kind + "-do-not-leak"
			t.Setenv("ANTHROPIC_API_KEY", "host-key-do-not-pass")
			h := mustStart(t, []step{initOK()}, agent.Options{InheritEnv: true, Credential: &agent.Credential{Kind: c.kind, Secret: secret}})
			_ = h.s.Close()
			h.drain()
			b, err := os.ReadFile(filepath.Join(h.dir, "credential.json"))
			if err != nil {
				t.Fatalf("the fake CLI read no credential: %v", err)
			}
			var got map[string]string
			if err := json.Unmarshal(b, &got); err != nil || got["var"] != c.fdVar || got["secret"] != secret || got["error"] != "" {
				t.Fatalf("credential read through the pipe: %s", b)
			}
			environ, err := os.ReadFile(filepath.Join(h.dir, "environ.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(environ), secret) || strings.Contains(string(environ), "host-key-do-not-pass") {
				t.Errorf("a credential is in the CLI's environment:\n%s", environ)
			}
			if !strings.Contains(string(environ), c.fdVar+"=3") {
				t.Errorf("%s=3 is not in the CLI's environment", c.fdVar)
			}
			if strings.Contains(strings.Join(h.args(), " "), secret) {
				t.Errorf("the credential is on the command line: %v", h.args())
			}
			if strings.Contains(fmt.Sprintf("%v %+v %#v", h.s.Info(), agent.Credential{Kind: c.kind, Secret: secret}, &agent.Credential{Kind: c.kind, Secret: secret}), secret) {
				t.Error("formatting a credential shows the secret")
			}
		})
	}
}

func TestCredentialRefusals(t *testing.T) {
	for name, opts := range map[string]agent.Options{
		"unknown kind":  {Credential: &agent.Credential{Kind: "login", Secret: "x"}},
		"empty secret":  {Credential: &agent.Credential{Kind: agent.CredentialAPIKey, Secret: "  "}},
		"two lines":     {Credential: &agent.Credential{Kind: agent.CredentialAPIKey, Secret: "a\nb"}},
		"too long":      {Credential: &agent.Credential{Kind: agent.CredentialAPIKey, Secret: strings.Repeat("a", 4097)}},
		"also in Env":   {Credential: &agent.Credential{Kind: agent.CredentialOAuthToken, Secret: "x"}, Env: map[string]string{"ANTHROPIC_API_KEY": "y"}},
		"fd var in Env": {Credential: &agent.Credential{Kind: agent.CredentialOAuthToken, Secret: "x"}, Env: map[string]string{"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR": "4"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := start(t, nil, []step{initOK()}, opts, claudecode.Extra{}); err == nil {
				t.Fatal("started")
			} else if strings.Contains(err.Error(), "x\"") {
				t.Fatalf("the error shows the secret: %v", err)
			}
		})
	}
}
