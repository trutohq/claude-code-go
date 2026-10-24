package claudecode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trutohq/claude-code-go/agent"
)

const (
	// maxLineBytes bounds one stdout line. Longer lines are delivered
	// truncated, as KindUnknown with a ParseError.
	maxLineBytes = 64 << 20
	// drainGrace is how long the reader may keep draining stdout after the
	// CLI exits before leftover children holding the pipe are killed.
	drainGrace = 2 * time.Second
	// markerSweepBound bounds the search for processes the session left.
	markerSweepBound = 2 * time.Second
	// stderrTailLines is how much stderr an ExitError carries.
	stderrTailLines = 40
	stderrLineMax   = 4 << 10
)

// Info describes a started session.
type Info struct {
	// Binary is the resolved CLI executable that runs.
	Binary string
	// Version is the CLI version (from --version and the initialize reply).
	Version string
	// VersionWarning is set when the version is untested and the driver
	// allows it.
	VersionWarning string
	// PID is the CLI process id.
	PID int
	// PermissionMode is the mode the CLI reports after initialize.
	PermissionMode string
	// Initialize is the raw initialize reply (commands, models, account...).
	Initialize json.RawMessage
}

// Session is a running Claude Code process. It implements agent.Session.
type Session struct {
	opts  agent.Options
	extra Extra
	info  Info

	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *os.File

	writeMu     sync.Mutex
	inputClosed atomic.Bool

	q *eventQueue

	mu          sync.Mutex
	sessionID   string
	model       string
	pending     map[string]pendingRequest
	inflight    map[string]*inflightRequest
	readerEnded bool
	lastErrText string
	reqCounter  int

	hookFns    map[string]hookEntry
	hookConfig map[string]any // nil when there are no hooks
	servers    map[string]*toolServer

	baseCtx    context.Context
	baseCancel context.CancelFunc

	// marker is this session's SessionMarkerEnv value.
	marker string
	host   string

	readerDone chan struct{}
	stderrDone chan struct{}
	exited     chan struct{}
	done       chan struct{}
	waitErr    error

	stderrMu   sync.Mutex
	stderrTail []string

	shutdownOnce sync.Once
}

var _ agent.Session = (*Session)(nil)

type hookEntry struct {
	fn    HookFunc
	event string
}

// inflightRequest is a control request from the CLI being served. Compared
// by pointer, so a handler only removes its own entry even if the CLI reuses
// a request id.
type inflightRequest struct {
	cancel context.CancelFunc
}

type pendingRequest struct {
	ch      chan controlReply
	subtype string
}

type controlReply struct {
	resp json.RawMessage
	err  error
}

// StartSession starts claude with agent-neutral options plus Claude-specific
// extras. ctx bounds the whole session: cancelling it stops the CLI.
func (d *Driver) StartSession(ctx context.Context, opts agent.Options, extra Extra) (*Session, error) {
	if err := validate(opts); err != nil {
		return nil, err
	}
	if opts.WorkDir != "" {
		abs, err := filepath.Abs(opts.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("claudecode: work dir: %w", err)
		}
		opts.WorkDir = abs
	}
	if extra.InitTimeout <= 0 {
		extra.InitTimeout = defaultInitTimeout
	}
	if extra.ShutdownGrace <= 0 {
		extra.ShutdownGrace = defaultShutdownGrace
	}
	binary, err := ResolveBinary(opts.BinaryPath)
	if err != nil {
		return nil, err
	}
	marker := randHex(16)
	env := buildEnv(os.Environ(), opts, marker)
	version, err := d.Version(ctx, binary, env)
	if err != nil {
		return nil, err
	}
	warning, err := d.checkVersion(binary, version)
	if err != nil {
		return nil, err
	}
	args, err := buildArgs(opts)
	if err != nil {
		return nil, err
	}

	s := &Session{
		opts:       opts,
		extra:      extra,
		info:       Info{Binary: binary, Version: version, VersionWarning: warning},
		q:          newEventQueue(),
		sessionID:  firstNonEmpty(opts.SessionID, opts.Resume),
		model:      opts.Model,
		marker:     marker,
		pending:    map[string]pendingRequest{},
		inflight:   map[string]*inflightRequest{},
		hookFns:    map[string]hookEntry{},
		servers:    map[string]*toolServer{},
		readerDone: make(chan struct{}),
		stderrDone: make(chan struct{}),
		exited:     make(chan struct{}),
		done:       make(chan struct{}),
	}
	s.baseCtx, s.baseCancel = context.WithCancel(context.Background())
	s.host, _ = os.Hostname()
	for _, srv := range opts.ToolServers {
		s.servers[srv.Name] = newToolServer(srv)
	}

	s.registerHooks()
	if err := s.spawn(binary, args, env); err != nil {
		s.baseCancel()
		s.q.close()
		return nil, err
	}

	go func() {
		select {
		case <-ctx.Done():
			s.shutdown()
		case <-s.done:
		}
	}()

	err = s.initialize(ctx)
	if err == nil && s.info.Version != version {
		// The binary changed between --version and the spawn.
		s.info.VersionWarning, err = d.checkVersion(binary, s.info.Version)
	}
	if err != nil {
		_ = s.Close()
		if werr := s.Wait(); werr != nil {
			err = fmt.Errorf("%w (%v)", err, werr)
		}
		// Nobody will read the events of a session that never started.
		for range s.q.out {
		}
		return nil, err
	}
	return s, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func (s *Session) spawn(binary string, args, env []string) error {
	inR, inW, err := os.Pipe()
	if err != nil {
		return err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return err
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = s.opts.WorkDir
	cmd.Env = env
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW
	setProcAttrs(cmd)
	err = cmd.Start()
	// The child has its own copies now (or failed to start).
	inR.Close()
	outW.Close()
	errW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		return fmt.Errorf("claudecode: start %s: %w", binary, err)
	}
	s.cmd, s.stdin, s.stdout, s.stderr = cmd, inW, outR, errR
	s.info.PID = cmd.Process.Pid

	go s.readStdout()
	go s.readStderr()
	go s.waitProcess()
	return nil
}

// waitProcess reaps the CLI, cleans up its process group and finishes the
// session.
func (s *Session) waitProcess() {
	err := s.cmd.Wait()
	close(s.exited)
	// Give the reader a moment to drain what the CLI wrote before exiting.
	select {
	case <-s.readerDone:
	case <-time.After(drainGrace):
	}
	// Background commands the agent started outlive the CLI otherwise, and
	// may hold the output pipes open. The group kill gets the CLI's direct
	// children; the marker sweep gets the ones it started with setsid.
	killGroup(s.cmd.Process.Pid)
	killMarked(s.marker, markerSweepBound)
	s.stdout.Close()
	s.stderr.Close()
	<-s.readerDone
	<-s.stderrDone
	s.closeInput()
	// Callbacks still running (a person who has not answered yet) see their
	// context cancelled. They are not waited for: one that ignores its
	// context must not hold up Close and Wait. A late reply finds input
	// closed and is dropped.
	s.baseCancel()
	s.waitErr = s.exitError(err)
	s.q.close()
	close(s.done)
}

func (s *Session) exitError(err error) error {
	if err == nil {
		return nil
	}
	s.mu.Lock()
	result := s.lastErrText
	s.mu.Unlock()
	s.stderrMu.Lock()
	tail := strings.Join(s.stderrTail, "\n")
	s.stderrMu.Unlock()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &agent.ExitError{
			Code:   ee.ExitCode(),
			Signal: exitSignal(ee.ProcessState),
			Stderr: tail,
			Result: result,
		}
	}
	return fmt.Errorf("claudecode: wait: %w", err)
}

// registerHooks assigns callback ids and builds the initialize hooks config.
// It runs before the CLI starts, so hookFns is read-only afterwards.
func (s *Session) registerHooks() {
	if len(s.extra.Hooks) == 0 {
		return
	}
	cfg := map[string]any{}
	n := 0
	for event, matchers := range s.extra.Hooks {
		var list []map[string]any
		for _, m := range matchers {
			var ids []string
			for _, fn := range m.Callbacks {
				id := fmt.Sprintf("hook_%d", n)
				n++
				s.hookFns[id] = hookEntry{fn: fn, event: event}
				ids = append(ids, id)
			}
			entry := map[string]any{"matcher": nil, "hookCallbackIds": ids}
			if m.Matcher != "" {
				entry["matcher"] = m.Matcher
			}
			if m.Timeout > 0 {
				entry["timeout"] = m.Timeout.Seconds()
			}
			list = append(list, entry)
		}
		if len(list) > 0 {
			cfg[event] = list
		}
	}
	s.hookConfig = cfg
}

// initialize performs the handshake: it registers hooks and learns the CLI's
// version, permission mode and capabilities. In-process MCP servers are
// initialized by the CLI while this is in flight.
func (s *Session) initialize(ctx context.Context) error {
	req := map[string]any{"subtype": "initialize", "hooks": s.hookConfig}
	ictx, cancel := context.WithTimeout(ctx, s.extra.InitTimeout)
	defer cancel()
	resp, err := s.request(ictx, req)
	if err != nil {
		return fmt.Errorf("claudecode: initialize: %w", err)
	}
	var r struct {
		Version        string `json:"claude_code_version"`
		PermissionMode string `json:"current_permission_mode"`
	}
	_ = json.Unmarshal(resp, &r)
	s.info.Initialize = resp
	s.info.PermissionMode = r.PermissionMode
	if r.Version != "" {
		s.info.Version = r.Version
	}
	return nil
}

// ClaudeInfo returns what is known about the running CLI.
func (s *Session) ClaudeInfo() Info { return s.info }

// Info implements agent.Session.
func (s *Session) Info() agent.SessionInfo {
	s.mu.Lock()
	model := s.model
	s.mu.Unlock()
	return agent.SessionInfo{Agent: Name, Version: s.info.Version, Model: model, Host: s.host, PID: s.info.PID}
}

// SessionID implements agent.Session.
func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Events implements agent.Session.
func (s *Session) Events() <-chan agent.Event { return s.q.out }

// Send implements agent.Session.
func (s *Session) Send(ctx context.Context, msg agent.UserMessage) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var content any = msg.Text
	if len(msg.Content) > 0 {
		content = msg.Content
	}
	line := map[string]any{
		"type":               "user",
		"session_id":         "",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
	}
	if msg.Priority != agent.PriorityDefault {
		line["priority"] = string(msg.Priority)
	}
	return s.write(ctx, line)
}

// Interrupt implements agent.Session.
func (s *Session) Interrupt(ctx context.Context) error {
	_, err := s.request(ctx, map[string]any{"subtype": "interrupt"})
	return err
}

// SetPermissionMode changes the permission mode of the running session.
func (s *Session) SetPermissionMode(ctx context.Context, mode string) error {
	_, err := s.request(ctx, map[string]any{"subtype": "set_permission_mode", "mode": mode})
	return err
}

// SetModel changes the model of the running session.
func (s *Session) SetModel(ctx context.Context, model string) error {
	_, err := s.request(ctx, map[string]any{"subtype": "set_model", "model": model})
	return err
}

// Request sends any control request (fields must include "subtype") and
// returns the CLI's reply payload.
func (s *Session) Request(ctx context.Context, fields map[string]any) (json.RawMessage, error) {
	if st, _ := fields["subtype"].(string); st == "" {
		return nil, errors.New("claudecode: control request needs a subtype")
	}
	return s.request(ctx, fields)
}

// CloseInput implements agent.Session.
func (s *Session) CloseInput() error {
	s.closeInput()
	return nil
}

func (s *Session) closeInput() {
	if s.inputClosed.CompareAndSwap(false, true) {
		// Not under writeMu: a write blocked on a full pipe must not hold
		// up closing. Closing unblocks it with an error.
		s.stdin.Close()
	}
}

// Close implements agent.Session: close stdin, then SIGTERM, then SIGKILL the
// process group, each after the shutdown grace period.
func (s *Session) Close() error {
	s.shutdown()
	<-s.done
	return nil
}

func (s *Session) shutdown() {
	s.shutdownOnce.Do(func() {
		go func() {
			s.closeInput()
			grace := s.extra.ShutdownGrace
			select {
			case <-s.exited:
				return
			case <-time.After(grace):
			}
			terminateGroup(s.cmd.Process)
			select {
			case <-s.exited:
				return
			case <-time.After(grace):
			}
			killGroup(s.cmd.Process.Pid)
			_ = s.cmd.Process.Kill()
		}()
	})
}

// Wait implements agent.Session.
func (s *Session) Wait() error {
	<-s.done
	return s.waitErr
}

// Done is closed once the session has fully ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// write sends one JSON line to the CLI and records it as a KindSent event.
// The event is queued before the write, so the transcript never shows a
// reply before what caused it; a failed write adds a KindDriver event. ctx
// bounds a write blocked on a full pipe; a write cut off part way leaves a
// broken line in the CLI's input, so input is then closed.
func (s *Session) write(ctx context.Context, v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.inputClosed.Load() {
		return agent.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ev := agent.Event{Kind: agent.KindSent, Raw: b, At: time.Now()}
	ev.Type, _ = v["type"].(string)
	if r, ok := v["request"].(map[string]any); ok {
		ev.Subtype, _ = r["subtype"].(string)
	}
	if r, ok := v["response"].(map[string]any); ok {
		ev.Subtype, _ = r["subtype"].(string)
	}
	s.q.push(ev)

	finish := s.deadlineFrom(ctx)
	n, werr := s.stdin.Write(append(b, '\n'))
	finish()
	if werr == nil {
		return nil
	}
	s.q.push(agent.Event{Kind: agent.KindDriver, Subtype: "write_failed", Raw: b, At: time.Now(), Err: werr})
	if errors.Is(werr, os.ErrDeadlineExceeded) {
		cause := ctx.Err()
		if cause == nil {
			cause = context.DeadlineExceeded
		}
		if n > 0 {
			s.closeInput()
			return fmt.Errorf("%w: partial line written, input closed", cause)
		}
		return cause
	}
	return fmt.Errorf("%w: %v", agent.ErrClosed, werr)
}

// deadlineFrom applies ctx's deadline and cancellation to the next stdin
// write; the returned func clears it.
func (s *Session) deadlineFrom(ctx context.Context) func() {
	if ctx.Done() == nil {
		return func() {}
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = s.stdin.SetWriteDeadline(dl)
	}
	var mu sync.Mutex
	done := false
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			_ = s.stdin.SetWriteDeadline(time.Unix(1, 0))
		}
	})
	return func() {
		stop()
		mu.Lock()
		done = true
		_ = s.stdin.SetWriteDeadline(time.Time{})
		mu.Unlock()
	}
}

func (s *Session) request(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	if s.readerEnded {
		s.mu.Unlock()
		return nil, agent.ErrClosed
	}
	s.reqCounter++
	id := fmt.Sprintf("req_%d_%s", s.reqCounter, randHex(4))
	ch := make(chan controlReply, 1)
	subtype, _ := req["subtype"].(string)
	s.pending[id] = pendingRequest{ch: ch, subtype: subtype}
	s.mu.Unlock()
	drop := func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}
	if err := s.write(ctx, map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		drop()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ControlError is an error reply to a control request the driver sent.
type ControlError struct {
	Subtype string
	Message string
}

func (e *ControlError) Error() string {
	return fmt.Sprintf("claudecode: %s refused: %s", e.Subtype, e.Message)
}

func (s *Session) readStdout() {
	defer func() {
		s.mu.Lock()
		s.readerEnded = true
		pending := s.pending
		s.pending = map[string]pendingRequest{}
		s.mu.Unlock()
		for _, p := range pending {
			p.ch <- controlReply{err: agent.ErrClosed}
		}
		close(s.readerDone)
	}()
	br := bufio.NewReaderSize(s.stdout, 256<<10)
	for {
		line, err := readLine(br, maxLineBytes)
		if errors.Is(err, errLineTooLong) {
			s.q.push(agent.Event{
				Kind: agent.KindUnknown, Raw: line, At: time.Now(),
				ParseError: fmt.Errorf("line longer than %d bytes, truncated", maxLineBytes),
			})
			continue
		}
		if len(line) > 0 {
			s.handleLine(line)
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) handleLine(raw []byte) {
	ev, env := parseLine(raw, time.Now())
	if ev.Kind == "" {
		return // blank line
	}
	s.mu.Lock()
	if ev.SessionID != "" && (s.sessionID == "" || ev.Kind == agent.KindInit) {
		s.sessionID = ev.SessionID
	}
	if ev.Init != nil && ev.Init.Model != "" {
		s.model = ev.Init.Model
	}
	if ev.Kind == agent.KindResult && ev.Result != nil {
		if ev.Result.IsError {
			s.lastErrText = errorResultText(ev.Result)
		} else {
			s.lastErrText = ""
		}
	}
	s.mu.Unlock()
	// The line is queued before any reply is written, so the transcript
	// shows a request before its response.
	s.q.push(ev)

	switch env.Type {
	case "control_response":
		s.routeResponse(raw)
	case "control_request":
		s.serveRequest(raw)
	case "control_cancel_request":
		s.mu.Lock()
		r := s.inflight[env.RequestID]
		s.mu.Unlock()
		if r != nil {
			r.cancel()
		}
	}
}

func errorResultText(r *agent.Result) string {
	if len(r.Errors) > 0 {
		return strings.Join(r.Errors, "; ")
	}
	if t := strings.TrimSpace(r.Text); t != "" {
		return t
	}
	if r.Subtype != "" && r.Subtype != "success" {
		return r.Subtype
	}
	return "error result"
}

func (s *Session) routeResponse(raw []byte) {
	var m struct {
		Response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
			Error     string          `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	s.mu.Lock()
	p, ok := s.pending[m.Response.RequestID]
	delete(s.pending, m.Response.RequestID)
	s.mu.Unlock()
	if !ok {
		return
	}
	ch := p.ch
	if m.Response.Subtype == "error" {
		msg := m.Response.Error
		if msg == "" {
			msg = "unknown error"
		}
		ch <- controlReply{err: &ControlError{Subtype: p.subtype, Message: msg}}
		return
	}
	ch <- controlReply{resp: m.Response.Response}
}

// serveRequest answers a control request from the CLI on its own goroutine,
// so a person taking an hour to answer blocks nothing else.
//
// A request whose id cannot be read gets no reply: there is nothing to
// address one to. A request with an id but an unreadable body gets an error
// reply, which the CLI treats as a denial. A control line over maxLineBytes
// is truncated, so it is never served.
func (s *Session) serveRequest(raw []byte) {
	var m struct {
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
	}
	if json.Unmarshal(raw, &m) != nil || m.RequestID == "" {
		return
	}
	var head struct {
		Subtype string `json:"subtype"`
	}
	_ = json.Unmarshal(m.Request, &head)

	ctx, cancel := context.WithCancel(s.baseCtx)
	entry := &inflightRequest{cancel: cancel}
	s.mu.Lock()
	s.inflight[m.RequestID] = entry
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			if s.inflight[m.RequestID] == entry {
				delete(s.inflight, m.RequestID)
			}
			s.mu.Unlock()
			cancel()
		}()
		resp, err := s.dispatch(ctx, head.Subtype, m.Request)
		if ctx.Err() != nil {
			// Withdrawn by the CLI (control_cancel_request) or the session
			// is ending: nobody is waiting for an answer.
			return
		}
		var reply map[string]any
		if err != nil {
			reply = map[string]any{"subtype": "error", "request_id": m.RequestID, "error": err.Error()}
		} else {
			reply = map[string]any{"subtype": "success", "request_id": m.RequestID, "response": resp}
		}
		_ = s.write(context.Background(), map[string]any{"type": "control_response", "response": reply})
	}()
}

func (s *Session) dispatch(ctx context.Context, subtype string, req json.RawMessage) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp, err = nil, fmt.Errorf("%s handler panicked: %v", subtype, r)
		}
	}()
	switch subtype {
	case "":
		return nil, errors.New("malformed control request: no subtype")
	case "can_use_tool":
		return s.canUseTool(ctx, req)
	case "hook_callback":
		return s.hookCallback(ctx, req)
	case "mcp_message":
		var m struct {
			ServerName string          `json:"server_name"`
			Message    json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(req, &m); err != nil || m.ServerName == "" || len(m.Message) == 0 {
			return nil, errors.New("missing server_name or message for MCP request")
		}
		return map[string]any{"mcp_response": s.handleMCP(ctx, m.ServerName, m.Message)}, nil
	default:
		return nil, fmt.Errorf("unsupported control request subtype: %s", subtype)
	}
}

// AskUserQuestionTool is the CLI's built-in question tool.
const AskUserQuestionTool = "AskUserQuestion"

type canUseToolRequest struct {
	ToolName       string          `json:"tool_name"`
	Input          json.RawMessage `json:"input"`
	ToolUseID      string          `json:"tool_use_id"`
	DisplayName    string          `json:"display_name"`
	Title          string          `json:"title"`
	Description    string          `json:"description"`
	DecisionReason string          `json:"decision_reason"`
	BlockedPath    string          `json:"blocked_path"`
	AgentID        string          `json:"agent_id"`
}

func (s *Session) canUseTool(ctx context.Context, raw json.RawMessage) (any, error) {
	var r canUseToolRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("bad can_use_tool request: %w", err)
	}
	if s.opts.Permission == nil {
		return nil, errors.New("no permission handler is configured")
	}
	input := r.Input
	if len(input) == 0 || string(input) == "null" {
		input = json.RawMessage("{}")
	}
	req := agent.PermissionRequest{
		ToolName:       r.ToolName,
		ToolUseID:      r.ToolUseID,
		Input:          input,
		DisplayName:    r.DisplayName,
		Title:          r.Title,
		Description:    r.Description,
		DecisionReason: r.DecisionReason,
		BlockedPath:    r.BlockedPath,
		AgentID:        r.AgentID,
		Raw:            raw,
	}
	if r.ToolName == AskUserQuestionTool {
		var q struct {
			Questions []agent.Question `json:"questions"`
		}
		if json.Unmarshal(input, &q) == nil {
			req.Questions = q.Questions
		}
	}
	dec, err := s.opts.Permission(ctx, req)
	if err != nil {
		return nil, err
	}
	if req.IsQuestion() && dec.Allow && len(dec.Answers) == 0 && !hasKey(dec.UpdatedInput, "answers") {
		return nil, errors.New("a question for a person was allowed without answers; " +
			"answer it with agent.Answer, or deny it")
	}
	return permissionResponse(input, dec)
}

// permissionResponse converts a decision to the CLI's reply. An allow always
// carries updatedInput (the original input when unchanged), as the SDKs do;
// answers to a question are returned inside it as "answers".
func permissionResponse(input json.RawMessage, dec agent.PermissionDecision) (map[string]any, error) {
	if !dec.Allow {
		msg := dec.Message
		if msg == "" {
			msg = "Denied"
		}
		out := map[string]any{"behavior": "deny", "message": msg}
		if dec.Interrupt {
			out["interrupt"] = true
		}
		return out, nil
	}
	updated := input
	if len(dec.UpdatedInput) > 0 {
		if !json.Valid(dec.UpdatedInput) {
			return nil, errors.New("updated input is not valid JSON")
		}
		updated = dec.UpdatedInput
	}
	if dec.Answers != nil {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(updated, &obj); err != nil || obj == nil {
			return nil, errors.New("answers need an object input")
		}
		// One label is sent as a string, several as an array (the CLI
		// accepts both; an array keeps labels that contain ", " intact).
		answers := map[string]any{}
		for q, labels := range dec.Answers {
			switch len(labels) {
			case 0:
			case 1:
				answers[q] = labels[0]
			default:
				answers[q] = labels
			}
		}
		a, err := json.Marshal(answers)
		if err != nil {
			return nil, err
		}
		obj["answers"] = a
		if updated, err = json.Marshal(obj); err != nil {
			return nil, err
		}
	}
	out := map[string]any{"behavior": "allow", "updatedInput": updated}
	if len(dec.UpdatedPermissions) > 0 {
		if !json.Valid(dec.UpdatedPermissions) {
			return nil, errors.New("updated permissions are not valid JSON")
		}
		out["updatedPermissions"] = dec.UpdatedPermissions
	}
	return out, nil
}

func (s *Session) hookCallback(ctx context.Context, raw json.RawMessage) (any, error) {
	var r struct {
		CallbackID string          `json:"callback_id"`
		Input      json.RawMessage `json:"input"`
		ToolUseID  string          `json:"tool_use_id"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("bad hook_callback request: %w", err)
	}
	e, ok := s.hookFns[r.CallbackID]
	if !ok {
		return nil, fmt.Errorf("no hook callback found for ID: %s", r.CallbackID)
	}
	out, err := runHook(ctx, e.fn, HookInput{CallbackID: r.CallbackID, ToolUseID: r.ToolUseID, Input: r.Input})
	if err == nil && len(out) > 0 {
		var obj map[string]json.RawMessage
		if json.Unmarshal(out, &obj) != nil || obj == nil {
			err = errors.New("hook output must be a JSON object")
		}
	}
	if err != nil {
		if e.event == "PreToolUse" {
			// The CLI reads a failed hook as "no opinion" and runs the
			// tool; a gate that broke must close instead.
			return map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       "deny",
				"permissionDecisionReason": "hook failed: " + err.Error(),
			}}, nil
		}
		return nil, err
	}
	if len(out) == 0 {
		return map[string]any{}, nil
	}
	return json.RawMessage(out), nil
}

func runHook(ctx context.Context, fn HookFunc, in HookInput) (out json.RawMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("hook panicked: %v", r)
		}
	}()
	return fn(ctx, in)
}

func hasKey(obj json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(obj, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

func (s *Session) readStderr() {
	defer close(s.stderrDone)
	br := bufio.NewReaderSize(s.stderr, 64<<10)
	for {
		line, err := readLine(br, maxLineBytes)
		if len(line) > 0 {
			s.stderrMu.Lock()
			t := string(line)
			if len(t) > stderrLineMax {
				t = t[:stderrLineMax] + "..."
			}
			s.stderrTail = append(s.stderrTail, t)
			if len(s.stderrTail) > stderrTailLines {
				s.stderrTail = s.stderrTail[len(s.stderrTail)-stderrTailLines:]
			}
			s.stderrMu.Unlock()
			s.q.push(agent.Event{Kind: agent.KindStderr, Raw: line, At: time.Now()})
		}
		if err != nil && !errors.Is(err, errLineTooLong) {
			return
		}
	}
}

var errLineTooLong = errors.New("line too long")

// readLine reads one newline-terminated line of any length up to max bytes
// (longer lines are truncated and reported with errLineTooLong). The trailing
// newline and carriage return are removed. At EOF it returns the final
// unterminated line, if any, with the error.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		frag, err := r.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(frag) > max {
				buf = append(buf, frag[:max-len(buf)]...)
				tooLong = true
			} else {
				buf = append(buf, frag...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		buf = trimEOL(buf)
		if tooLong {
			if err != nil {
				return buf, err
			}
			return buf, errLineTooLong
		}
		if err != nil {
			if err == io.EOF && len(buf) == 0 {
				return nil, io.EOF
			}
			return buf, err
		}
		return buf, nil
	}
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// eventQueue is an unbounded FIFO in front of the Events channel, so control
// requests (permission prompts, tool calls) are served even while the
// consumer is busy.
type eventQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []agent.Event
	closed bool
	out    chan agent.Event
}

func newEventQueue() *eventQueue {
	q := &eventQueue{out: make(chan agent.Event, 64)}
	q.cond = sync.NewCond(&q.mu)
	go q.pump()
	return q
}

func (q *eventQueue) push(ev agent.Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.items = append(q.items, ev)
	q.cond.Signal()
}

func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *eventQueue) pump() {
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.items) == 0 {
			q.mu.Unlock()
			close(q.out)
			return
		}
		ev := q.items[0]
		q.items[0] = agent.Event{}
		q.items = q.items[1:]
		q.mu.Unlock()
		q.out <- ev
	}
}
