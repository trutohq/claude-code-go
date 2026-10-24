package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/trutohq/claude-code-go/agent"
)

// envelope holds the fields every stdout line may carry.
type envelope struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	SessionID       string  `json:"session_id"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	RequestID       string  `json:"request_id"`
}

// parseLine turns one stdout line into an Event. Raw is always kept; the
// typed fields are best effort. A blank line returns an Event with no Kind.
func parseLine(raw []byte, at time.Time) (agent.Event, envelope) {
	ev := agent.Event{Raw: raw, At: at}
	var env envelope
	if len(bytes.TrimSpace(raw)) == 0 {
		return agent.Event{}, env
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		ev.Kind = agent.KindUnknown
		ev.ParseError = fmt.Errorf("not a JSON object: %w", err)
		return ev, env
	}
	ev.Type, ev.Subtype, ev.SessionID = env.Type, env.Subtype, env.SessionID
	if env.ParentToolUseID != nil {
		ev.ParentToolUseID = *env.ParentToolUseID
	}
	var err error
	switch env.Type {
	case "system":
		switch env.Subtype {
		case "init":
			ev.Kind = agent.KindInit
			ev.Init, err = parseInit(raw)
		case "session_state_changed":
			ev.Kind = agent.KindSessionState
			var st struct {
				State string `json:"state"`
			}
			err = json.Unmarshal(raw, &st)
			ev.State = st.State
		default:
			ev.Kind = agent.KindSystem
		}
	case "assistant":
		ev.Kind = agent.KindAssistant
		ev.Message, err = parseMessage(raw)
	case "user":
		ev.Kind = agent.KindUser
		ev.Message, err = parseMessage(raw)
	case "result":
		ev.Kind = agent.KindResult
		ev.Result, err = parseResult(raw)
	case "rate_limit_event":
		ev.Kind = agent.KindRateLimit
		ev.RateLimit, err = parseRateLimit(raw)
	case "stream_event":
		ev.Kind = agent.KindStreamDelta
	case "control_request", "control_response", "control_cancel_request":
		ev.Kind = agent.KindControl
	default:
		ev.Kind = agent.KindUnknown
	}
	if err != nil {
		ev.ParseError = fmt.Errorf("%s line: %w", env.Type, err)
	}
	return ev, env
}

func parseInit(raw []byte) (*agent.Init, error) {
	var m struct {
		CWD            string                  `json:"cwd"`
		Model          string                  `json:"model"`
		PermissionMode string                  `json:"permissionMode"`
		Version        string                  `json:"claude_code_version"`
		Tools          []string                `json:"tools"`
		MCPServers     []agent.MCPServerStatus `json:"mcp_servers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &agent.Init{
		CWD: m.CWD, Model: m.Model, PermissionMode: m.PermissionMode,
		Version: m.Version, Tools: m.Tools, MCPServers: m.MCPServers,
	}, nil
}

func parseMessage(raw []byte) (*agent.Message, error) {
	var m struct {
		Message struct {
			ID      string          `json:"id"`
			Role    string          `json:"role"`
			Model   string          `json:"model"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	msg := &agent.Message{ID: m.Message.ID, Role: m.Message.Role, Model: m.Message.Model}
	c := bytes.TrimSpace(m.Message.Content)
	switch {
	case len(c) == 0 || string(c) == "null":
	case c[0] == '"':
		// A user turn may carry plain string content.
		var text string
		if err := json.Unmarshal(c, &text); err != nil {
			return nil, err
		}
		msg.Content = []agent.ContentBlock{{Type: "text", Text: text}}
	default:
		if err := json.Unmarshal(c, &msg.Content); err != nil {
			return nil, err
		}
	}
	return msg, nil
}

func parseResult(raw []byte) (*agent.Result, error) {
	var m struct {
		Subtype        string                 `json:"subtype"`
		IsError        bool                   `json:"is_error"`
		Result         string                 `json:"result"`
		Errors         []json.RawMessage      `json:"errors"`
		NumTurns       int                    `json:"num_turns"`
		DurationMS     int64                  `json:"duration_ms"`
		TotalCostUSD   float64                `json:"total_cost_usd"`
		StopReason     string                 `json:"stop_reason"`
		TerminalReason string                 `json:"terminal_reason"`
		Deferred       *agent.DeferredToolUse `json:"deferred_tool_use"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	r := &agent.Result{
		Subtype: m.Subtype, IsError: m.IsError, Text: m.Result, NumTurns: m.NumTurns,
		DurationMS: m.DurationMS, TotalCostUSD: m.TotalCostUSD, StopReason: m.StopReason,
		TerminalReason: m.TerminalReason, Deferred: m.Deferred,
	}
	for _, e := range m.Errors {
		var s string
		if json.Unmarshal(e, &s) == nil {
			r.Errors = append(r.Errors, s)
			continue
		}
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e, &obj) == nil && obj.Message != "" {
			r.Errors = append(r.Errors, obj.Message)
			continue
		}
		r.Errors = append(r.Errors, string(e))
	}
	return r, nil
}

func parseRateLimit(raw []byte) (*agent.RateLimit, error) {
	var m struct {
		Info *struct {
			Status        string   `json:"status"`
			ResetsAt      *float64 `json:"resetsAt"`
			Type          string   `json:"rateLimitType"`
			Utilization   *float64 `json:"utilization"`
			OverageStatus string   `json:"overageStatus"`
		} `json:"rate_limit_info"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Info == nil || m.Info.Status == "" {
		return nil, fmt.Errorf("missing rate_limit_info.status")
	}
	rl := &agent.RateLimit{
		Status:        agent.RateLimitStatus(m.Info.Status),
		Type:          m.Info.Type,
		Utilization:   m.Info.Utilization,
		OverageStatus: m.Info.OverageStatus,
	}
	if m.Info.ResetsAt != nil && *m.Info.ResetsAt > 0 {
		// Unix seconds.
		sec := int64(*m.Info.ResetsAt)
		rl.ResetsAt = time.Unix(sec, 0).UTC()
	}
	return rl, nil
}
