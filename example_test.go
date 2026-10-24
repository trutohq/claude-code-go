package claudecode_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	claudecode "github.com/trutohq/claude-code-go"
	"github.com/trutohq/claude-code-go/agent"
)

// Start a session, send one prompt, answer permission prompts, and read the
// events until the agent is idle. This example needs a Claude login and the
// tested CLI version, so it is compiled but not run by `go test`.
func Example() {
	ctx := context.Background()
	d := claudecode.New() // refuses CLI versions other than claudecode.TestedVersion

	s, err := d.Start(ctx, agent.Options{
		WorkDir:        "/path/to/repo",
		Model:          "sonnet",
		PermissionMode: "default",
		AllowedTools:   []string{"Read", "Grep", "Glob"},
		Permission: func(ctx context.Context, req agent.PermissionRequest) (agent.PermissionDecision, error) {
			if req.IsQuestion() {
				// AskUserQuestion: pick the first option of each question.
				answers := map[string]string{}
				for _, q := range req.Questions {
					if len(q.Options) > 0 {
						answers[q.Question] = q.Options[0].Label
					}
				}
				return agent.Answer(answers), nil
			}
			if req.ToolName == "Edit" || req.ToolName == "Write" {
				return agent.Allow(), nil
			}
			return agent.Deny("only file edits are allowed"), nil
		},
	})
	if err != nil {
		log.Fatal(err) // a *claudecode.VersionError if the CLI version is untested
	}
	defer s.Close()

	if err := s.Send(ctx, agent.UserMessage{Text: "Fix the failing test in parser_test.go"}); err != nil {
		log.Fatal(err)
	}
	var sawResult bool
	for ev := range s.Events() {
		switch ev.Kind {
		case agent.KindAssistant:
			fmt.Print(ev.Message.Text())
		case agent.KindResult:
			sawResult = true
		case agent.KindSessionState:
			// A result ends a turn; idle after a result means nothing is left.
			if ev.State == "idle" && sawResult {
				_ = s.CloseInput()
			}
		}
	}
	if err := s.Wait(); err != nil {
		log.Fatal(err)
	}
}

// Expose an in-process tool to the agent. The CLI sees it as mcp__tickets__comment.
func Example_toolServer() {
	tickets := agent.ToolServer{Name: "tickets", Version: "1.0.0", Tools: []agent.Tool{{
		Name:        "comment",
		Description: "Add a comment to the ticket this run works on.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		Handler: func(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
			var in struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(call.Arguments, &in); err != nil {
				return agent.ToolResult{}, err // reported to the model as a failed tool call
			}
			return agent.TextResult("comment added"), nil
		},
	}}}
	_ = agent.Options{
		WorkDir:      "/path/to/repo",
		ToolServers:  []agent.ToolServer{tickets},
		AllowedTools: []string{"mcp__tickets__comment"}, // no permission prompt for it
	}
}

// Gate tools with an in-process PreToolUse hook. A hook that errors or panics
// denies the tool (fails closed).
func Example_hooks() {
	ctx := context.Background()
	noForcePush := func(ctx context.Context, in claudecode.HookInput) (json.RawMessage, error) {
		var p struct {
			ToolInput struct {
				Command string `json:"command"`
			} `json:"tool_input"`
		}
		if err := json.Unmarshal(in.Input, &p); err != nil {
			return nil, err
		}
		if strings.Contains(p.ToolInput.Command, "push --force") {
			return json.RawMessage(`{"hookSpecificOutput":{"hookEventName":"PreToolUse",` +
				`"permissionDecision":"deny","permissionDecisionReason":"force push is not allowed"}}`), nil
		}
		return nil, nil // no opinion
	}
	s, err := claudecode.New().StartSession(ctx, agent.Options{WorkDir: "/path/to/repo"}, claudecode.Extra{
		Hooks: map[string][]claudecode.HookMatcher{
			"PreToolUse": {{Matcher: "Bash", Callbacks: []claudecode.HookFunc{noForcePush}}},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
}
