package claudecode

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/trutohq/claude-code-go/agent"
)

// In-process tools are served as "SDK" MCP servers: the server is declared to
// the CLI in --mcp-config as {"type": "sdk", "name": ...}, and the CLI then
// sends every JSON-RPC message for it as an mcp_message control request. The
// reply carries the JSON-RPC response as "mcp_response". This file is a
// minimal MCP server for tools: initialize, ping, tools/list and tools/call.

const (
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603

	defaultMCPProtocolVersion = "2025-06-18"
	toolUseIDMetaKey          = "claudecode/toolUseId"
)

type toolServer struct {
	srv    agent.ToolServer
	byName map[string]agent.Tool
}

func newToolServer(srv agent.ToolServer) *toolServer {
	ts := &toolServer{srv: srv, byName: map[string]agent.Tool{}}
	for _, t := range srv.Tools {
		ts.byName[t.Name] = t
	}
	return ts
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func rpcID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func rpcOK(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rpcID(id), "result": result}
}

func rpcFail(id json.RawMessage, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rpcID(id), "error": rpcError{Code: code, Message: msg}}
}

// handleMCP answers one JSON-RPC message for an in-process server.
func (s *Session) handleMCP(ctx context.Context, server string, raw json.RawMessage) map[string]any {
	var msg rpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return rpcFail(nil, -32700, "parse error")
	}
	ts := s.servers[server]
	if ts == nil {
		return rpcFail(msg.ID, rpcMethodNotFound, fmt.Sprintf("Server '%s' not found", server))
	}
	if msg.Method == "" || len(msg.ID) == 0 || string(msg.ID) == "null" {
		// A notification or a response: nothing to answer, but the control
		// request that carried it still expects an acknowledgement.
		return map[string]any{"jsonrpc": "2.0", "result": map[string]any{}}
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		pv := p.ProtocolVersion
		if pv == "" {
			pv = defaultMCPProtocolVersion
		}
		version := ts.srv.Version
		if version == "" {
			version = "1.0.0"
		}
		return rpcOK(msg.ID, map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ts.srv.Name, "version": version},
		})
	case "ping":
		return rpcOK(msg.ID, map[string]any{})
	case "tools/list":
		tools := make([]map[string]any, 0, len(ts.srv.Tools))
		for _, t := range ts.srv.Tools {
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": schema,
			})
		}
		return rpcOK(msg.ID, map[string]any{"tools": tools})
	case "tools/call":
		return s.callTool(ctx, ts, msg)
	default:
		return rpcFail(msg.ID, rpcMethodNotFound, "Method not found: "+msg.Method)
	}
}

func (s *Session) callTool(ctx context.Context, ts *toolServer, msg rpcMessage) map[string]any {
	var p struct {
		Name      string                     `json:"name"`
		Arguments json.RawMessage            `json:"arguments"`
		Meta      map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return rpcFail(msg.ID, rpcInvalidParams, "invalid tools/call params")
	}
	tool, ok := ts.byName[p.Name]
	if !ok {
		return rpcFail(msg.ID, rpcInvalidParams, "Unknown tool: "+p.Name)
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	var toolUseID string
	if v, ok := p.Meta[toolUseIDMetaKey]; ok {
		_ = json.Unmarshal(v, &toolUseID)
	}
	res, err := runTool(ctx, tool, agent.ToolCall{
		Server: ts.srv.Name, Tool: p.Name, ToolUseID: toolUseID, Arguments: args,
	})
	if err != nil {
		// Tool failures are results the model can read, not protocol errors.
		res = agent.ToolResult{IsError: true, Content: []agent.ToolContent{{Type: "text", Text: err.Error()}}}
	}
	content := res.Content
	if content == nil {
		content = []agent.ToolContent{}
	}
	out := map[string]any{"content": content}
	if res.IsError {
		out["isError"] = true
	}
	return rpcOK(msg.ID, out)
}

func runTool(ctx context.Context, tool agent.Tool, call agent.ToolCall) (res agent.ToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool %s panicked: %v", call.Tool, r)
		}
	}()
	return tool.Handler(ctx, call)
}
