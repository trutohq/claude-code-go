# claude-code-go

A Go driver for the [Claude Code](https://docs.anthropic.com/en/docs/claude-code) CLI. It starts `claude` as a subprocess and speaks the stream-json control protocol that Anthropic's Agent SDKs use: streamed messages, permission prompts, questions to a person, hook callbacks, in-process MCP tools, interrupts and resume.

It is written for programs that run coding agents unattended: the defaults load no settings files, pass the host's environment through an allowlist, and kill everything the agent started when the session ends.

**This is not an official Anthropic SDK.** It is an independent implementation of the same protocol. Anthropic's own SDKs are for [Python](https://github.com/anthropics/claude-agent-sdk-python) and [TypeScript](https://github.com/anthropics/claude-agent-sdk-typescript).

```
go get github.com/trutohq/claude-code-go
```

No dependencies outside the standard library. Go 1.24 or later.

## Status

Pre-1.0. The API can change between minor versions until 1.0.

| | |
|---|---|
| Tested CLI version | **2.1.296** (`claudecode.TestedVersion`) |
| Platforms | Linux (full process cleanup), macOS and other Unix (process-group cleanup only). Windows builds but only the CLI process itself is stopped. CI runs on Linux. |
| Protocol notes | [docs/protocol.md](docs/protocol.md): every flag, line type and control request the driver relies on, with what was verified and how |

## Version pinning

The stream-json control protocol is not a documented stable API. A renamed field can mean a question that is never answered or a permission granted on the wrong input, with no error anywhere. So the driver refuses CLI versions it was not tested with:

1. `Start` resolves the `claude` binary (symlinks followed) and runs `<binary> --version`. The result is cached per file.
2. A version other than `TestedVersion` fails with a `*claudecode.VersionError`.
3. The `initialize` reply's `claude_code_version` is checked again, in case the binary changed in between.
4. The driver sets `DISABLE_AUTOUPDATER=1`, so the CLI does not replace itself during a run.

To run an untested version anyway, set `Driver.AllowUntestedVersion`. The session then starts, and `Session.ClaudeInfo().VersionWarning` says why it should not have.

To keep a version pinned, point `Options.BinaryPath` at the versioned binary (`~/.local/share/claude/versions/<version>` for the native installer) and not at the `claude` symlink, which moves on update.

A new CLI version is supported by re-running the protocol checks against it and raising `TestedVersion` (see "Re-verifying on a CLI upgrade" in [docs/protocol.md](docs/protocol.md)).

## Quick start

```go
import (
	claudecode "github.com/trutohq/claude-code-go"
	"github.com/trutohq/claude-code-go/agent"
)

d := claudecode.New() // refuses CLI versions other than claudecode.TestedVersion
s, err := d.Start(ctx, agent.Options{
	WorkDir:        "/path/to/repo",
	Model:          "sonnet",
	PermissionMode: "default",
	AllowedTools:   []string{"Read", "Grep", "Glob"},
	Permission: func(ctx context.Context, req agent.PermissionRequest) (agent.PermissionDecision, error) {
		if req.ToolName == "Edit" || req.ToolName == "Write" {
			return agent.Allow(), nil
		}
		return agent.Deny("only file edits are allowed"), nil
	},
})
if err != nil {
	return err // a *claudecode.VersionError if the CLI version is untested
}
defer s.Close()

if err := s.Send(ctx, agent.UserMessage{Text: "Fix the failing test in parser_test.go"}); err != nil {
	return err
}
var sawResult bool
for ev := range s.Events() {
	switch ev.Kind {
	case agent.KindAssistant:
		fmt.Print(ev.Message.Text())
	case agent.KindResult:
		sawResult = true
	case agent.KindSessionState:
		if ev.State == "idle" && sawResult {
			s.CloseInput() // nothing left to do: let the CLI exit
		}
	}
}
return s.Wait() // nil, or an *agent.ExitError with the stderr tail
```

The full example is in [example_test.go](example_test.go).

Things to know:

- **A `result` ends a turn, not the session.** Background tasks can wake the agent for more turns. Wait for `KindSessionState` with state `idle` after a result before you close input. Once input is closed, permission prompts, hooks and in-process tools stop working.
- **Drain `Events()` until it is closed.** It is an unbounded queue, so control requests are served even when you read slowly, but memory grows until you read.
- **Every line is kept raw.** `Event.Raw` holds the exact bytes of each line the CLI wrote, each line the driver wrote (`KindSent`) and each stderr line (`KindStderr`). The typed fields are a convenience. Unknown line types arrive as `KindUnknown` and are never dropped, so you can store a complete transcript.
- `Send` works while the agent is working. `UserMessage.Priority` (`now`, `next`, `later`) says when the message is delivered. `Interrupt` stops the current turn and keeps the session.
- `Session.SetModel`, `Session.SetPermissionMode` and `Session.Request` (any control request, for example `get_context_usage`) are on the concrete `*claudecode.Session`, which `Driver.StartSession` returns.

## Permission prompts and questions

When `Options.Permission` is set, the CLI asks it before each tool call that is not already allowed (`AllowedTools`, the permission mode). When it is nil, the CLI denies anything that would prompt and does not offer `AskUserQuestion`.

- The callback runs on its own goroutine and can wait as long as a person needs. Its context is cancelled if the CLI withdraws the request or the session ends.
- `agent.Allow()`, `agent.Deny(msg)`, or a `PermissionDecision` with `UpdatedInput` (change the tool's input) or `Interrupt` (deny and stop the turn).
- An error or a panic in the callback is a denial.
- `AskUserQuestion` arrives as a request with `req.IsQuestion()` true and `req.Questions` parsed. Answer it with `agent.Answer(map[question]label)` or `agent.AnswerMulti`. An allow without answers is refused, so a blanket "allow everything" callback cannot skip a question meant for a person.
- No question timeout was found on this path on 2.1.296, so a person can take as long as they need (details in [docs/protocol.md](docs/protocol.md)).

## Hooks

`Driver.StartSession` takes `claudecode.Extra`, with in-process hook callbacks by event name:

```go
s, err := claudecode.New().StartSession(ctx, opts, claudecode.Extra{
	Hooks: map[string][]claudecode.HookMatcher{
		"PreToolUse": {{Matcher: "Bash", Callbacks: []claudecode.HookFunc{noForcePush}}},
	},
})
```

A `HookFunc` returns the hook output in the CLI's JSON format, or nil for no opinion.

- **PreToolUse hooks fail closed.** The CLI reads a failed hook callback as "no opinion" and runs the tool. The driver turns an error, a panic or an output that is not a JSON object from a PreToolUse hook into an explicit `deny`. For other events a failure is reported to the CLI as an error.
- `claudecode.DeferToolUse(reason)` builds a PreToolUse output that stops the CLI before the tool runs. The `result` then carries `Deferred`, and you resume the session later. A permission callback that waits is usually simpler.

## In-process MCP tools

`Options.ToolServers` exposes Go functions to the agent as MCP tools, with no extra process or socket. A server named `tickets` with a tool `comment` is seen by the model as `mcp__tickets__comment`.

```go
tickets := agent.ToolServer{Name: "tickets", Version: "1.0.0", Tools: []agent.Tool{{
	Name:        "comment",
	Description: "Add a comment to the ticket this run works on.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	Handler: func(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
		return agent.TextResult("comment added"), nil
	},
}}}
opts.ToolServers = []agent.ToolServer{tickets}
opts.AllowedTools = append(opts.AllowedTools, "mcp__tickets__comment") // else each call prompts
```

- In-process tools are permission-checked like any other tool. List them in `AllowedTools` to skip the prompt.
- A handler error or panic becomes a tool result with `isError: true`, which the model reads. The session goes on.
- On 2.1.296 the CLI defers MCP tool schemas: the model first loads the tool with `ToolSearch`, which costs one extra turn. Allow for it in `MaxTurns`.
- External MCP servers go in `Options.MCPConfig` (paths or JSON, as for `--mcp-config`). Set `StrictMCPConfig` to ignore every server you did not list, including the user's and any claude.ai connectors.

## Resume

- `Options.SessionID` (a UUID) starts a new conversation with that id. `Options.Resume` continues an existing one. `Session.SessionID()` returns the id the CLI reports.
- The CLI resumes from the project directory of the working directory it runs in. `claudecode.SessionFile(configDir, cwd, sessionID)` returns where that conversation file is, so you can copy it when the working directory or the machine changes. It does not reproduce the CLI's hashed names for working directories longer than 200 characters.
- Resuming does not restore the permission mode. The driver passes `--permission-mode` on every start (`default` when empty).
- Never run two processes on one session id.

## Safety notes

The defaults assume the agent works on a repository you do not fully trust, and that prompt injection is possible.

- **No settings files.** The driver always passes `--setting-sources=` (empty) unless `Options.SettingSources` says otherwise. A `.claude/settings.json` committed to the repository cannot add `permissions.allow` rules or hooks that get around your permission callback. Project agents, skills and the repository's `.mcp.json` are not loaded either. Load agents and skills you trust through `Options.PluginDirs`. Add `user` only deliberately, with a dedicated `Options.ConfigDir`.
- **The permission mode is always passed.** Without it the CLI takes `defaultMode` from the user's settings, which can be `bypassPermissions`.
- **Environment allowlist.** Only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TZ`, `TERM`, `TMPDIR`, the `XDG_*` directories, proxy variables and CA-bundle variables are inherited. Your program's tokens and cloud keys do not reach the agent, where `env` in a Bash tool would show them. `Options.Env` adds variables (unfiltered, so anything secret there is visible to the agent). `Options.InheritEnv` passes everything. In both modes the variables a parent Claude Code session sets for its children are removed, so a session started from inside Claude Code does not attach to it.
- **One login per config directory.** `Options.ConfigDir` sets `CLAUDE_CONFIG_DIR`, which is never inherited, so the login is always chosen explicitly.
- **Process cleanup.** The CLI runs in its own process group. `Close` closes input, waits 5 seconds (`Extra.ShutdownGrace`), sends SIGTERM to the group, waits again and sends SIGKILL. When the CLI exits, the group is killed. The CLI starts its Bash shells and hooks with `setsid`, outside the group, so the driver also puts a random marker, `CCGO_AGENT_SESSION=<id>`, in the CLI's environment. On Linux it then kills every process whose environment carries that marker, including a dev server an agent left running. A process that clears its environment, or runs as another user, escapes. For full containment run each session in its own cgroup or container.
- **Values are passed as `--flag=value`**, so a value that starts with `-` cannot be read as another flag.

## Testing

```
go test -race ./...
```

The tests run against a fake CLI: the test binary runs itself again as a scripted `claude` (`fake_test.go`), so they need no login and no network.

`TestRealCLI` runs the installed CLI once (Haiku, a few turns, a permission prompt, a question and an in-process tool call; about one cent on your own login):

```
CCGO_CLAUDE_IT=1 go test -count=1 -run TestRealCLI -v .
```

`CCGO_CLAUDE_BIN` selects the binary, and `CCGO_CLAUDE_ALLOW_UNTESTED=1` lets it run a version other than `TestedVersion`.

## Licence

MIT, see [LICENSE](LICENSE).

The protocol handling was ported from Anthropic's MIT-licensed [Python Agent SDK](https://github.com/anthropics/claude-agent-sdk-python). [NOTICE](NOTICE) has the attribution and that SDK's licence. "Claude" and "Claude Code" are trademarks of Anthropic, PBC.
