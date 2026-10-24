// Package agent is an agent-neutral interface for driving a coding agent CLI
// as a subprocess. Package claudecode (the module root) implements it for
// Claude Code; the interface is kept separate so a host program can be
// written against it and tested with fakes, and so other agent CLIs can be
// driven the same way.
//
// A Driver starts a Session. A Session is one running agent process: it streams
// Events (each carrying the raw line it came from, so the host keeps the
// complete transcript), accepts follow-up user messages, can be interrupted,
// and asks the host program, through a PermissionFunc, whenever the agent
// needs a person to approve a tool or answer a question.
//
// There is no AI in here: drivers are plumbing between a host program and a
// CLI.
//
// Some fields are Claude-shaped because Claude Code is the first driver:
// UserMessage.Content, PermissionDecision.UpdatedPermissions, the permission
// mode names, the Priority values, SettingSources and ContentBlock follow
// Claude Code's formats. Other drivers reject what they do not support rather
// than ignore it.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Driver starts agent sessions. Implementations live in their own packages
// (github.com/trutohq/claude-code-go for Claude Code).
type Driver interface {
	// Name identifies the agent, e.g. "claude-code".
	Name() string
	// Start launches the agent and completes any startup handshake. The
	// session lives until ctx is cancelled, Close is called, or the agent
	// exits on its own; cancelling ctx stops the agent.
	Start(ctx context.Context, opts Options) (Session, error)
}

// Session is one running agent process.
//
// Events must be drained until the channel is closed; it is closed after the
// process has exited and every line has been delivered.
type Session interface {
	// SessionID is the agent's conversation id, used to resume later. It may
	// be empty until the agent reports it (KindInit).
	SessionID() string
	// Events streams everything the agent emits, plus every line the driver
	// writes to it (KindSent), in order of observation.
	Events() <-chan Event
	// Send delivers a user message, equivalent to a person typing a follow-up
	// in the chat. It may be sent while the agent is working.
	Send(ctx context.Context, msg UserMessage) error
	// Interrupt stops the agent's current turn. The session stays usable.
	Interrupt(ctx context.Context) error
	// CloseInput tells the agent no more messages are coming. It finishes
	// what it is doing and exits. Permission prompts and in-process tools
	// stop working once input is closed.
	CloseInput() error
	// Close stops the agent: input is closed, then it is signalled if it does
	// not exit within a grace period. It returns once the process is gone.
	Close() error
	// Wait blocks until the process has exited and returns nil for a clean
	// exit or an *ExitError.
	Wait() error
	// Info says which agent, version, model and machine run the session.
	Info() SessionInfo
}

// SessionInfo identifies what runs a session, for labelling its output.
type SessionInfo struct {
	Agent   string // driver name, e.g. "claude-code"
	Version string // agent CLI version
	Model   string // model in use, as far as known (requested, then reported)
	Host    string // machine name
	PID     int    // agent process id
}

// Options configures one session. Fields an agent does not support are
// rejected by its driver rather than silently ignored.
type Options struct {
	// WorkDir is the directory the agent works in.
	WorkDir string
	// SessionID starts a new conversation with this id (a UUID). Mutually
	// exclusive with Resume.
	SessionID string
	// Resume continues an existing conversation by id.
	Resume string
	// Model and Effort select the model and reasoning effort.
	Model  string
	Effort string
	// AppendSystemPrompt / AppendSystemPromptFile add to the agent's default
	// system prompt.
	AppendSystemPrompt     string
	AppendSystemPromptFile string
	// AllowedTools are tool rules approved without asking; DisallowedTools
	// are refused without asking.
	AllowedTools    []string
	DisallowedTools []string
	// PermissionMode is the agent's permission mode. Drivers always pass it
	// explicitly, because the user's own settings may default to something
	// looser.
	PermissionMode string
	// MaxTurns bounds the number of agentic turns (0 = no limit).
	MaxTurns int
	// MCPConfig lists external MCP server configurations (file paths or
	// JSON strings) in the agent's own format.
	MCPConfig []string
	// StrictMCPConfig ignores every MCP server not listed in MCPConfig or
	// ToolServers (user and project servers, claude.ai connectors).
	StrictMCPConfig bool
	// ToolServers are in-process tools the host program exposes to the
	// agent ("ask a person", "attach a screenshot", "update a ticket").
	ToolServers []ToolServer
	// Permission answers tool-approval prompts and questions. When nil,
	// anything that would prompt is denied by the agent itself.
	Permission PermissionFunc
	// IncludePartialMessages streams token deltas (KindStreamDelta).
	IncludePartialMessages bool
	// Env is added to the agent's environment, overriding inherited values.
	// Values here are never filtered: anything secret put here is visible
	// to the agent.
	Env map[string]string
	// InheritEnv passes the host program's whole environment to the agent
	// (minus variables a parent agent session sets). By default only a
	// small allowlist is inherited (PATH, HOME, locale, terminal, temp dir,
	// proxies), so the host's own secrets never reach the agent.
	InheritEnv bool
	// SettingSources lists which of the agent's settings files are loaded
	// ("user", "project", "local" for Claude Code). Empty loads none, so
	// permission rules and hooks committed to the repo the agent works on
	// cannot widen what it may do. Opt into "user" deliberately, with a
	// dedicated ConfigDir.
	SettingSources []string
	// PluginDirs are directories the agent loads as plugins for this
	// session only (Claude Code's --plugin-dir): how a run gets its agent
	// definitions and skills while no setting sources are loaded.
	// Absolute paths of existing directories.
	PluginDirs []string
	// BinaryPath is the agent executable; empty means look it up on PATH.
	BinaryPath string
	// ConfigDir selects the agent's config/login directory.
	ConfigDir string
	// ExtraArgs are appended to the command line verbatim.
	ExtraArgs []string
}

// Priority says when a message sent mid-run is delivered.
type Priority string

const (
	PriorityDefault Priority = ""      // the agent's default (next)
	PriorityNow     Priority = "now"   // interrupts the current step
	PriorityNext    Priority = "next"  // at the next opportunity
	PriorityLater   Priority = "later" // once the agent is idle
)

// Valid reports whether p is a known priority.
func (p Priority) Valid() bool {
	switch p {
	case PriorityDefault, PriorityNow, PriorityNext, PriorityLater:
		return true
	}
	return false
}

// UserMessage is a message from a person to the agent.
type UserMessage struct {
	// Text is the message. Ignored when Content is set.
	Text string
	// Content is a raw array of content blocks (text, image, ...) in the
	// agent's message format, for messages that are not plain text.
	Content json.RawMessage
	// Priority controls delivery while the agent is working.
	Priority Priority
}

// Validate checks the message before it is sent.
func (m UserMessage) Validate() error {
	if !m.Priority.Valid() {
		return fmt.Errorf("agent: unknown message priority %q", m.Priority)
	}
	if len(m.Content) == 0 && strings.TrimSpace(m.Text) == "" {
		return errors.New("agent: empty user message")
	}
	if len(m.Content) > 0 {
		var blocks []json.RawMessage
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return fmt.Errorf("agent: message content must be a JSON array of blocks: %w", err)
		}
	}
	return nil
}

// Kind classifies an Event.
type Kind string

const (
	KindInit         Kind = "init"          // the agent started a turn loop; Init is set
	KindAssistant    Kind = "assistant"     // model output; Message is set
	KindUser         Kind = "user"          // tool results and echoed user turns; Message is set
	KindResult       Kind = "result"        // a turn finished; Result is set
	KindRateLimit    Kind = "rate_limit"    // usage-limit state changed; RateLimit is set
	KindSessionState Kind = "session_state" // running / idle / requires_action; State is set
	KindStreamDelta  Kind = "stream_delta"  // partial output (IncludePartialMessages)
	KindSystem       Kind = "system"        // other agent status lines; Subtype says which
	KindControl      Kind = "control"       // control-protocol traffic from the agent
	KindSent         Kind = "sent"          // a line the driver wrote to the agent
	KindStderr       Kind = "stderr"        // a line the agent wrote to stderr
	KindUnknown      Kind = "unknown"       // unrecognised or unparsable line; Raw is kept
	KindDriver       Kind = "driver"        // a note from the driver itself; Subtype says which, Err why
)

// Event is one line of agent traffic. Raw always holds the exact line (without
// its trailing newline); the typed fields are a convenience parse of it.
type Event struct {
	Kind Kind
	// Type and Subtype are the agent's own names for the line.
	Type    string
	Subtype string
	Raw     []byte
	At      time.Time

	SessionID string
	// ParentToolUseID is set when the line comes from a subagent.
	ParentToolUseID string

	Init      *Init
	Message   *Message
	Result    *Result
	RateLimit *RateLimit
	State     string
	// ParseError is set when Raw could not be parsed (Kind is KindUnknown)
	// or a known type had an unexpected shape (Kind is kept, typed field nil).
	ParseError error
	// Err is set on KindDriver events: what went wrong.
	Err error
}

// Init describes the session as the agent reports it.
type Init struct {
	CWD            string
	Model          string
	PermissionMode string
	Version        string
	Tools          []string
	MCPServers     []MCPServerStatus
}

// MCPServerStatus is one MCP server as the agent sees it.
type MCPServerStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Message is an assistant or user message.
type Message struct {
	ID      string
	Role    string
	Model   string
	Content []ContentBlock
}

// Text joins the message's text blocks.
func (m *Message) Text() string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// ContentBlock is one block of a message. Fields are set according to Type
// ("text", "thinking", "tool_use", "tool_result", ...).
type ContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`    // tool_use
	Name     string          `json:"name,omitempty"`  // tool_use
	Input    json.RawMessage `json:"input,omitempty"` // tool_use
	// ToolUseID, Content and IsError describe a tool_result.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Result ends a turn.
type Result struct {
	Subtype        string
	IsError        bool
	Text           string
	Errors         []string
	NumTurns       int
	DurationMS     int64
	TotalCostUSD   float64
	StopReason     string
	TerminalReason string
	// Deferred is set when a hook deferred a tool call: the agent stopped and
	// the session must be resumed once the answer is known.
	Deferred *DeferredToolUse
}

// DeferredToolUse is a tool call the agent stopped on.
type DeferredToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// RateLimitStatus is the usage-limit state.
type RateLimitStatus string

const (
	RateLimitAllowed        RateLimitStatus = "allowed"
	RateLimitAllowedWarning RateLimitStatus = "allowed_warning"
	RateLimitRejected       RateLimitStatus = "rejected"
)

// RateLimit is a usage-limit state change. When Status is RateLimitRejected
// the run should pause until ResetsAt.
type RateLimit struct {
	Status RateLimitStatus
	// Type names the window, e.g. "five_hour", "seven_day".
	Type     string
	ResetsAt time.Time // zero when not reported
	// Utilization is the fraction used, 0..1; nil when not reported.
	Utilization   *float64
	OverageStatus string
}

// PermissionFunc decides a permission prompt or answers a question. It runs
// on its own goroutine and may take as long as a person needs; ctx is
// cancelled if the agent withdraws the request or the session ends.
// Returning an error reports a failure to the agent (the tool is not run).
type PermissionFunc func(ctx context.Context, req PermissionRequest) (PermissionDecision, error)

// PermissionRequest asks whether a tool may run.
type PermissionRequest struct {
	ToolName  string
	ToolUseID string
	Input     json.RawMessage
	// Questions is set when the agent asks a person a question
	// (ToolName is the agent's question tool); answer with
	// PermissionDecision.Answers.
	Questions []Question
	// Human-readable context from the agent, any of which may be empty.
	DisplayName    string
	Title          string
	Description    string
	DecisionReason string
	BlockedPath    string
	// AgentID is set when a subagent asks.
	AgentID string
	// Raw is the whole request as the agent sent it.
	Raw json.RawMessage
}

// IsQuestion reports whether the request is a question for a person.
func (r PermissionRequest) IsQuestion() bool { return len(r.Questions) > 0 }

// Question is one question the agent asks a person.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	MultiSelect bool             `json:"multiSelect,omitempty"`
	Options     []QuestionOption `json:"options,omitempty"`
	// Raw is the question as the agent sent it, including fields not
	// modelled here (newer question kinds, bounds, units, placeholders).
	Raw json.RawMessage `json:"-"`
}

// QuestionOption is one offered answer.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// PermissionDecision answers a PermissionRequest.
type PermissionDecision struct {
	Allow bool
	// UpdatedInput replaces the tool's input when allowing; nil keeps it.
	UpdatedInput json.RawMessage
	// Answers answers a question request: question text -> the chosen
	// option labels (one for a single choice, several for a multi-select
	// question, or free text). Only used when Allow is true; an allow of a
	// question without answers is refused, so a blanket auto-allow cannot
	// skip a question meant for a person.
	Answers map[string][]string
	// Message tells the agent why it was denied.
	Message string
	// Interrupt stops the agent's turn as well as denying.
	Interrupt bool
	// UpdatedPermissions is a raw array of permission updates (agent format)
	// to apply along with an allow.
	UpdatedPermissions json.RawMessage
}

// Allow approves a request unchanged.
func Allow() PermissionDecision { return PermissionDecision{Allow: true} }

// Deny refuses a request with a reason for the agent.
func Deny(message string) PermissionDecision {
	return PermissionDecision{Message: message}
}

// Answer answers a question request with one answer per question.
func Answer(answers map[string]string) PermissionDecision {
	m := make(map[string][]string, len(answers))
	for q, a := range answers {
		m[q] = []string{a}
	}
	return PermissionDecision{Allow: true, Answers: m}
}

// AnswerMulti answers a question request, several labels per question
// allowed.
func AnswerMulti(answers map[string][]string) PermissionDecision {
	return PermissionDecision{Allow: true, Answers: answers}
}

// ToolServer is a named group of in-process tools exposed to the agent.
type ToolServer struct {
	// Name is the server name; the agent sees tools as
	// mcp__<Name>__<tool>. Letters, digits, '_' and '-' only.
	Name    string
	Version string
	Tools   []Tool
}

// Tool is one in-process tool.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object; nil means any object.
	InputSchema json.RawMessage
	Handler     ToolHandler
}

// ToolHandler runs a tool call. ctx is cancelled if the agent abandons the
// call or the session ends. A returned error is reported to the agent as a
// failed tool call (it does not end the session).
type ToolHandler func(ctx context.Context, call ToolCall) (ToolResult, error)

// ToolCall is one invocation of an in-process tool.
type ToolCall struct {
	Server    string
	Tool      string
	ToolUseID string
	Arguments json.RawMessage
}

// ToolResult is what a tool returns to the agent.
type ToolResult struct {
	Content []ToolContent
	IsError bool
}

// ToolContent is one piece of a tool result.
type ToolContent struct {
	Type     string `json:"type"` // "text" or "image"
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // base64, for images
	MIMEType string `json:"mimeType,omitempty"`
}

// TextResult is a tool result with one text block.
func TextResult(text string) ToolResult {
	return ToolResult{Content: []ToolContent{{Type: "text", Text: text}}}
}

// ExitError reports an agent process that exited unsuccessfully.
type ExitError struct {
	// Code is the exit code, or -1 when the process was killed by a signal.
	Code   int
	Signal string
	// Stderr is the tail of the agent's stderr.
	Stderr string
	// Result is the error text of the last error result, if the agent
	// reported one before exiting.
	Result string
}

func (e *ExitError) Error() string {
	var b strings.Builder
	if e.Signal != "" {
		fmt.Fprintf(&b, "agent killed by signal %s", e.Signal)
	} else {
		fmt.Fprintf(&b, "agent exited with code %d", e.Code)
	}
	if e.Result != "" {
		fmt.Fprintf(&b, ": %s", e.Result)
	}
	if s := strings.TrimSpace(e.Stderr); s != "" {
		fmt.Fprintf(&b, "\nstderr:\n%s", s)
	}
	return b.String()
}

// ErrClosed is returned by Session methods once the agent's input is closed
// or the process has exited.
var ErrClosed = errors.New("agent: session closed")

// UnmarshalJSON keeps the raw question alongside the parsed fields.
func (q *Question) UnmarshalJSON(b []byte) error {
	type plain Question
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*q = Question(p)
	q.Raw = append(json.RawMessage(nil), b...)
	return nil
}
