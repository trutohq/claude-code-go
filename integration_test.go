//go:build unix

package claudecode_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	claudecode "github.com/trutohq/claude-code-go"
	"github.com/trutohq/claude-code-go/agent"
)

// TestRealCLI runs the installed claude once: haiku, a few turns, one Write
// permission prompt answered through the driver, and one in-process tool
// call. It uses the machine's existing Claude login and costs about a cent.
//
//	CCGO_CLAUDE_IT=1 go test -run TestRealCLI -v .
//
// Optional: CCGO_CLAUDE_BIN selects the binary, and
// CCGO_CLAUDE_ALLOW_UNTESTED=1 lets it run a version other than
// TestedVersion (the upgrade check in docs/protocol.md).
func TestRealCLI(t *testing.T) {
	if os.Getenv("CCGO_CLAUDE_IT") != "1" {
		t.Skip("set CCGO_CLAUDE_IT=1 to run against the installed claude CLI")
	}
	work := t.TempDir()
	sid := newUUID(t)
	// To see whether the project's CLAUDE.md is loaded with no setting
	// sources (logged from the init line's memory_paths).
	if err := os.WriteFile(filepath.Join(work, "CLAUDE.md"), []byte("# Test project\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var prompts []agent.PermissionRequest
	perm := func(ctx context.Context, r agent.PermissionRequest) (agent.PermissionDecision, error) {
		mu.Lock()
		prompts = append(prompts, r)
		mu.Unlock()
		if r.ToolName == "Write" {
			return agent.Allow(), nil
		}
		if r.IsQuestion() {
			// A person picks the second option.
			return agent.Answer(map[string]string{r.Questions[0].Question: "Blue"}), nil
		}
		return agent.Deny("only Write is allowed in this test"), nil
	}
	notes := make(chan agent.ToolCall, 4)
	notesSrv := agent.ToolServer{Name: "notes", Version: "0.0.1", Tools: []agent.Tool{{
		Name:        "note",
		Description: "Record a short progress note.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		Handler: func(ctx context.Context, c agent.ToolCall) (agent.ToolResult, error) {
			notes <- c
			return agent.TextResult("noted"), nil
		},
	}}}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	d := claudecode.New()
	// For checking a new CLI version before bumping TestedVersion.
	d.AllowUntestedVersion = os.Getenv("CCGO_CLAUDE_ALLOW_UNTESTED") == "1"
	s, err := d.StartSession(ctx, agent.Options{
		WorkDir:         work,
		SessionID:       sid,
		Model:           "haiku",
		MaxTurns:        10,
		PermissionMode:  "default",
		Permission:      perm,
		ToolServers:     []agent.ToolServer{notesSrv},
		AllowedTools:    []string{"mcp__notes__note"},
		StrictMCPConfig: true,
		BinaryPath:      os.Getenv("CCGO_CLAUDE_BIN"),
		// The environment is not inherited by default, so the login is
		// passed explicitly: the machine's existing one.
		ConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"),
	}, claudecode.Extra{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close()
	info := s.ClaudeInfo()
	t.Logf("session info: %+v", s.Info())
	t.Logf("binary %s version %s pid %d permission mode %q", info.Binary, info.Version, info.PID, info.PermissionMode)
	if info.VersionWarning != "" {
		t.Logf("UNTESTED VERSION: %s", info.VersionWarning)
	}
	if info.PermissionMode != "default" {
		t.Errorf("permission mode after initialize = %q", info.PermissionMode)
	}

	prompt := "First use the AskUserQuestion tool to ask me one question: which color I prefer, with the two options Red and Blue.\n" +
		"Then use the Write tool to create the file hello.txt in the current directory containing exactly: hi\n" +
		"Then call the mcp__notes__note tool with text set to: wrote hello.txt\n" +
		"Then reply with only the color I chose, as a single word."
	if err := s.Send(ctx, agent.UserMessage{Text: prompt}); err != nil {
		t.Fatal(err)
	}

	counts := map[string]int{}
	var initEv, resultEv *agent.Event
	var states []string
	var rawLines int
	for done := false; !done; {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("events closed before the result")
			}
			if len(ev.Raw) > 0 {
				rawLines++
			}
			key := string(ev.Kind)
			if ev.Subtype != "" {
				key += "/" + ev.Subtype
			} else if ev.Type != "" && ev.Kind != agent.KindAssistant && ev.Kind != agent.KindUser {
				key += "/" + ev.Type
			}
			counts[key]++
			if ev.ParseError != nil {
				t.Errorf("parse error on %s: %v: %.300s", ev.Kind, ev.ParseError, ev.Raw)
			}
			switch ev.Kind {
			case agent.KindInit:
				e := ev
				initEv = &e
			case agent.KindSessionState:
				states = append(states, ev.State)
			case agent.KindRateLimit:
				t.Logf("rate limit: %+v", *ev.RateLimit)
			case agent.KindResult:
				e := ev
				resultEv = &e
				done = true
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the result")
		}
	}
	if initEv == nil || initEv.Init == nil {
		t.Fatal("no init event")
	}
	t.Logf("init: model %s, mode %s, version %s, %d tools, mcp %+v", initEv.Init.Model, initEv.Init.PermissionMode,
		initEv.Init.Version, len(initEv.Init.Tools), initEv.Init.MCPServers)
	var initKeys map[string]json.RawMessage
	_ = json.Unmarshal(initEv.Raw, &initKeys)
	t.Logf("init keys: %v", sortedKeys(initKeys))
	t.Logf("init memory_paths: %s", initKeys["memory_paths"])
	if s.SessionID() != sid {
		t.Errorf("session id %q, want %q", s.SessionID(), sid)
	}
	r := resultEv.Result
	t.Logf("result: subtype %s error %v turns %d cost $%.4f terminal %s text %q", r.Subtype, r.IsError, r.NumTurns,
		r.TotalCostUSD, r.TerminalReason, r.Text)
	if r.IsError {
		t.Errorf("error result: %+v", r)
	}
	if !strings.Contains(r.Text, "Blue") {
		t.Errorf("final text %q does not carry the answered color", r.Text)
	}
	if counts["assistant"] == 0 {
		t.Error("no assistant message")
	}

	mu.Lock()
	var writePrompt, question bool
	for _, p := range prompts {
		t.Logf("permission prompt: tool %s display %q description %q questions %+v", p.ToolName, p.DisplayName, p.Description, p.Questions)
		if p.ToolName == "Write" {
			writePrompt = true
		}
		if p.IsQuestion() {
			question = true
		}
	}
	mu.Unlock()
	if !question {
		t.Error("no AskUserQuestion round trip")
	}
	if !writePrompt {
		t.Error("no can_use_tool round trip for Write")
	}
	if b, err := os.ReadFile(filepath.Join(work, "hello.txt")); err != nil || strings.TrimSpace(string(b)) != "hi" {
		t.Errorf("hello.txt = %q, %v", b, err)
	}
	select {
	case c := <-notes:
		t.Logf("in-process tool call: %s/%s tool_use_id %s args %s", c.Server, c.Tool, c.ToolUseID, c.Arguments)
	default:
		t.Error("in-process tool was not called")
	}

	if err := s.CloseInput(); err != nil {
		t.Fatal(err)
	}
	for ev := range s.Events() {
		if len(ev.Raw) > 0 {
			rawLines++
		}
		key := string(ev.Kind)
		if ev.Subtype != "" {
			key += "/" + ev.Subtype
		}
		counts[key]++
		if ev.Kind == agent.KindSessionState {
			states = append(states, ev.State)
		}
	}
	if err := s.Wait(); err != nil {
		t.Errorf("wait: %v", err)
	}
	t.Logf("session states: %v", states)
	t.Logf("%d raw lines; event counts: %v", rawLines, counts)
}

func newUUID(t *testing.T) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRealCLICredential checks that the installed claude reads a Credential
// from the pipe: with a bogus token or key, an empty config dir and a
// scratch HOME (no login anywhere), the first message must fail with an
// authentication error (401), not "not logged in". The bogus credential is
// refused before any model call, so it costs nothing.
//
//	CCGO_CLAUDE_CRED_IT=1 [CCGO_CLAUDE_BIN=...] go test -run TestRealCLICredential -v .
func TestRealCLICredential(t *testing.T) {
	if os.Getenv("CCGO_CLAUDE_CRED_IT") != "1" {
		t.Skip("set CCGO_CLAUDE_CRED_IT=1 to run against the installed claude CLI")
	}
	// In the shapes the CLI expects; a token without the sk-ant-oat01-
	// prefix was not taken as one (2.1.296: "Not logged in").
	bogus := map[string]string{
		agent.CredentialOAuthToken: "sk-ant-oat01-bogus-0000000000000000000000000000000000000000",
		agent.CredentialAPIKey:     "sk-ant-api03-bogus-0000000000000000000000000000000000000000",
	}
	for _, kind := range []string{agent.CredentialOAuthToken, agent.CredentialAPIKey} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			cfg := filepath.Join(home, "config")
			if err := os.MkdirAll(cfg, 0o700); err != nil {
				t.Fatal(err)
			}
			d := claudecode.New()
			d.AllowUntestedVersion = os.Getenv("CCGO_CLAUDE_ALLOW_UNTESTED") == "1"
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			s, err := d.Start(ctx, agent.Options{
				WorkDir: t.TempDir(), SessionID: newUUID(t), Model: "haiku", MaxTurns: 1,
				BinaryPath: os.Getenv("CCGO_CLAUDE_BIN"), ConfigDir: cfg,
				Env:        map[string]string{"HOME": home},
				Credential: &agent.Credential{Kind: kind, Secret: bogus[kind]},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.Send(ctx, agent.UserMessage{Text: "Say hi."}); err != nil {
				t.Fatal(err)
			}
			for ev := range s.Events() {
				if ev.Kind == agent.KindInit {
					t.Logf("init: apiKeySource %s", gjson(ev.Raw, "apiKeySource"))
				}
				if ev.Kind != agent.KindResult {
					continue
				}
				t.Logf("result: %s", ev.Raw)
				if !strings.Contains(string(ev.Raw), `"api_error_status":401`) {
					t.Fatalf("not an authentication error: %s", ev.Raw)
				}
				if strings.Contains(strings.ToLower(string(ev.Raw)), "not logged in") {
					t.Fatalf("the CLI did not read the credential: %s", ev.Raw)
				}
				return
			}
			t.Fatal("no result")
		})
	}
}

func gjson(raw []byte, key string) string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return fmt.Sprint(m[key])
}
