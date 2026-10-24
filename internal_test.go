package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/trutohq/claude-code-go/agent"
)

func TestBuildArgsUsesEqualsForm(t *testing.T) {
	args, err := buildArgs(agent.Options{
		Resume: "--dangerous", Model: "-x", PermissionMode: "acceptEdits",
		DisallowedTools: []string{"Bash(rm *)", "WebFetch"}, AppendSystemPromptFile: "/p.md",
		Permission: func(context.Context, agent.PermissionRequest) (agent.PermissionDecision, error) {
			return agent.Allow(), nil
		},
		IncludePartialMessages: true, ExtraArgs: []string{"--fork-session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--input-format=stream-json", "--output-format=stream-json", "--verbose",
		"--resume=--dangerous", "--model=-x", "--append-system-prompt-file=/p.md",
		"--disallowedTools=Bash(rm *),WebFetch", "--permission-mode=acceptEdits", "--setting-sources=",
		"--permission-prompt-tool=stdio", "--include-partial-messages", "--fork-session"}
	if !slices.Equal(args, want) {
		t.Errorf("args =\n%q\nwant\n%q", args, want)
	}
	args, _ = buildArgs(agent.Options{})
	if !slices.Contains(args, "--permission-mode="+DefaultPermissionMode) || !slices.Contains(args, "--permission-prompts=none") ||
		!slices.Contains(args, "--setting-sources=") {
		t.Errorf("defaults = %q", args)
	}
	args, _ = buildArgs(agent.Options{SettingSources: []string{"user", "local"}})
	if !slices.Contains(args, "--setting-sources=user,local") {
		t.Errorf("setting sources = %q", args)
	}
}

func TestBuildEnv(t *testing.T) {
	base := []string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_BRIDGE_OWNER_ORG_UUID=x",
		"CLAUDE_CONFIG_DIR=/home/u/.claude", "HOME=/home/u", "DISABLE_AUTOUPDATER=0", "broken", "LC_ALL=C.UTF-8",
		"GITHUB_TOKEN=ghp_x", "AWS_SECRET_ACCESS_KEY=y", "TRACEPARENT=00-1", "GIT_CONFIG_PARAMETERS='a.b=c'",
		"CLAUDE_CODE_TMPDIR=/t", "BUN_OPTIONS=-x", "CLAUDE_CODE_INVOKED_SKILLS=s", "SHELL=/bin/zsh", "TMPDIR=/tmp/w",
		"CCGO_AGENT_SESSION=parent"}
	parse := func(env []string) map[string]string {
		m := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			if _, dup := m[k]; dup {
				t.Errorf("duplicate %s", k)
			}
			m[k] = v
		}
		return m
	}
	always := map[string]string{"CLAUDE_CODE_ENTRYPOINT": "sdk-cli", "DISABLE_AUTOUPDATER": "1",
		"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS": "1", "PWD": "/w", "CCGO_AGENT_SESSION": "m1",
		"HOME": "/h2", "GITHUB_TOKEN": "explicit"}
	opts := agent.Options{Env: map[string]string{"HOME": "/h2", "GITHUB_TOKEN": "explicit"}, WorkDir: "/w"}
	stripped := []string{"CLAUDECODE", "CLAUDE_CODE_BRIDGE_OWNER_ORG_UUID", "broken", "TRACEPARENT", "GIT_CONFIG_PARAMETERS",
		"CLAUDE_CODE_TMPDIR", "BUN_OPTIONS", "CLAUDE_CODE_INVOKED_SKILLS"}

	t.Run("allowlist", func(t *testing.T) {
		m := parse(buildEnv(base, opts, "m1"))
		for k, v := range always {
			if m[k] != v {
				t.Errorf("%s = %q, want %q", k, m[k], v)
			}
		}
		for k, v := range map[string]string{"PATH": "/bin", "LC_ALL": "C.UTF-8", "SHELL": "/bin/zsh", "TMPDIR": "/tmp/w"} {
			if m[k] != v {
				t.Errorf("%s = %q, want %q", k, m[k], v)
			}
		}
		for _, k := range append([]string{"AWS_SECRET_ACCESS_KEY", "CLAUDE_CONFIG_DIR"}, stripped...) {
			if _, ok := m[k]; ok {
				t.Errorf("%s inherited", k)
			}
		}
	})
	t.Run("inherit", func(t *testing.T) {
		o := opts
		o.InheritEnv = true
		o.ConfigDir = "/cfg"
		m := parse(buildEnv(base, o, "m1"))
		for k, v := range always {
			if m[k] != v {
				t.Errorf("%s = %q, want %q", k, m[k], v)
			}
		}
		if m["AWS_SECRET_ACCESS_KEY"] != "y" || m["CLAUDE_CONFIG_DIR"] != "/cfg" {
			t.Errorf("full inheritance missing: %v", m)
		}
		for _, k := range stripped {
			if _, ok := m[k]; ok {
				t.Errorf("%s inherited", k)
			}
		}
	})
}

func TestReadLine(t *testing.T) {
	long := strings.Repeat("a", 100)
	r := bufio.NewReaderSize(strings.NewReader("one\r\n"+long+"\nthree\n"+strings.Repeat("b", 40)+"\nlast"), 16)
	type res struct {
		line string
		err  error
	}
	var got []res
	for {
		l, err := readLine(r, 50)
		got = append(got, res{string(l), err})
		if err != nil && !errors.Is(err, errLineTooLong) {
			break
		}
	}
	want := []res{{"one", nil}, {long[:50], errLineTooLong}, {"three", nil}, {strings.Repeat("b", 40), nil}, {"last", io.EOF}}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestPermissionResponse(t *testing.T) {
	in := json.RawMessage(`{"questions":[{"question":"Q?"}]}`)
	out, err := permissionResponse(in, agent.Answer(map[string]string{"Q?": "A, B"}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if string(b) != `{"behavior":"allow","updatedInput":{"answers":{"Q?":"A, B"},"questions":[{"question":"Q?"}]}}` {
		t.Errorf("got %s", b)
	}
	out, err = permissionResponse(in, agent.AnswerMulti(map[string][]string{"Q?": {"Red, dark", "Blue"}, "Skipped?": nil}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(out)
	if string(b) != `{"behavior":"allow","updatedInput":{"answers":{"Q?":["Red, dark","Blue"]},"questions":[{"question":"Q?"}]}}` {
		t.Errorf("multi: got %s", b)
	}
	if _, err := permissionResponse(json.RawMessage(`[1]`), agent.Answer(map[string]string{"a": "b"})); err == nil {
		t.Error("answers merged into a non-object")
	}
	if _, err := permissionResponse(in, agent.PermissionDecision{Allow: true, UpdatedInput: json.RawMessage(`{bad`)}); err == nil {
		t.Error("invalid updated input accepted")
	}
	out, _ = permissionResponse(in, agent.PermissionDecision{})
	if out["behavior"] != "deny" || out["message"] != "Denied" {
		t.Errorf("deny = %v", out)
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{"2.1.296 (Claude Code)\n": "2.1.296", "v10.20.30": "10.20.30"} {
		if got, err := ParseVersion(in); err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseVersion("Claude Code"); err == nil {
		t.Error("no error for missing version")
	}
}
