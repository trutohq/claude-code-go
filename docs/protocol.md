# Claude Code control protocol, as implemented

The driver runs the `claude` CLI directly, speaking the same stream-json control protocol as
Anthropic's Agent SDKs. The code is package `claudecode` at the root of this module; the
agent-neutral interface it implements is package `agent`. This page is what that code relies on,
verified against the real CLI.

- **Tested CLI version: 2.1.296** (`claudecode.TestedVersion`), verified 2026-10-21 by hand probes
  and by `TestRealCLI`.
- Ported from the Python Agent SDK `claude-agent-sdk-python` 0.2.170 (commit `dd27b70`,
  2026-10-21): `_internal/query.py`, `_internal/transport/subprocess_cli.py`, `types.py`,
  `_internal/message_parser.py`. Cross-checked against `severity1/claude-agent-sdk-go` (commit
  `d167fbe`). Both MIT. No code was copied; the Go is written from scratch. See [NOTICE](../NOTICE).
- The protocol is **not a documented stable API**. That is why the version is pinned and checked.

## Process

```
claude -p --input-format=stream-json --output-format=stream-json --verbose
  --session-id=<uuid> | --resume=<id>
  --permission-mode=<mode>                    always passed (see gotchas)
  --setting-sources=<list>                    always passed; empty by default
  --permission-prompt-tool=stdio              when the host answers prompts
  --permission-prompts=none                   when it does not
  [--model= --effort= --max-turns= --append-system-prompt[-file]=
   --allowedTools= --disallowedTools= --mcp-config=... --strict-mcp-config
   --include-partial-messages --plugin-dir=... --settings=...]
```

- Every value is passed as `--flag=value`. Some flags take optional or variadic values, so in the
  two-token form a value starting with `-` would be parsed as another flag.
- `--mcp-config` may be repeated; each one is merged (verified with two configs).
- `--permission-prompt-tool`, `--append-system-prompt-file` and `--max-turns` are **hidden**: they
  are not in `claude --help` on 2.1.296, but they work.
- `--bare` is never used: it ignores subscription logins.
- Working directory: `cmd.Dir` and `PWD` are both set to the worktree.
- The CLI runs in its own **process group**. Close: close stdin, wait the grace period (5s),
  SIGTERM the group, wait again, SIGKILL.
- **Cleanup of what the agent left running.** When the CLI exits, two steps run:
  - The driver SIGKILLs the CLI's process group.
  - The CLI starts its Bash tool shells, hooks and pty host **detached** (`setsid`; binary
    strings show `{type:"bash",...,detached:!0,...}`), so those leave the group. The group kill
    does not reach them, nor a dev server such a shell started. To cover them, the driver puts a
    random marker in the CLI's environment, `CCGO_AGENT_SESSION=<id>`, which every child
    inherits (`setsid` does not remove it). On Linux it then scans `/proc/*/environ` for that
    exact entry and SIGKILLs every match, rescanning until none is left or 2s pass.
  - Limits: a process that scrubs its environment, or one owned by another user, escapes. The
    sweep is Linux-only; elsewhere only the group kill applies. Full containment (a cgroup per
    run) belongs to the host program.
  - Both kills happen after the CLI has been reaped, so in principle its pid (the group id) could
    be reused. That needs pid wrap-around within a couple of seconds, which is unlikely with a
    large `pid_max`.
- **Environment: an allowlist by default.** Only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`,
  `LANG`, `LC_*`, `TZ`, `TERM`, `TMPDIR`, `XDG_*` dirs, the proxy variables and the CA-bundle
  variables are inherited. The host program's own credentials (`GITHUB_TOKEN`, API tokens, cloud
  keys) never reach the agent, where `env` in Bash would show them to anything a prompt injection
  asks for.
  - `Options.Env` adds variables unfiltered. `Options.ConfigDir` sets `CLAUDE_CONFIG_DIR`, which
    is not inherited, so the login is always chosen explicitly.
  - `Options.InheritEnv` restores full inheritance.
  - In both modes, the variables a parent Claude Code session sets for its children are dropped.
    That is the CLI's own child-session list (`GIT_EDITOR`, `CLAUDECODE`, `AI_AGENT`,
    `CLAUDE_CODE_SESSION_ID`, `TRACEPARENT`, `CLAUDE_CODE_TMPDIR`, `GIT_CONFIG_PARAMETERS`,
    `BUN_OPTIONS`, `TEMP`, `TMP`, ...) minus `SHELL` and `TMPDIR`, plus `CLAUDE_CODE_ENTRYPOINT`,
    `CLAUDE_CODE_MESSAGING_*`, `CLAUDE_CODE_BRIDGE_*`, `CLAUDE_REMOTE_CONTROL_*`, `TRACESTATE` and
    the session marker. The full list is in `driver.go`.
  - The driver also sets these:
    - `CLAUDE_CODE_ENTRYPOINT=sdk-cli`. The CLI accepts a fixed set of entrypoints and groups
      `sdk-cli`, `sdk-ts` and `sdk-py` as "SDK"; `sdk-cli` is its non-language name.
    - `DISABLE_AUTOUPDATER=1`, so the pinned CLI never replaces itself mid-run.
    - `CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1`, for `session_state_changed` lines.
    - `CLAUDE_CONFIG_DIR=<Options.ConfigDir>` when set. Use one per login.
    - `CCGO_AGENT_SESSION=<random id>`, the cleanup marker described above (`SessionMarkerEnv`).
- **Settings files: none by default.** `--setting-sources=` (empty) loads no user, project or
  local settings, so a `.claude/settings.json` committed to the repo the agent works on cannot add
  `permissions.allow` rules or hooks that bypass the `PermissionFunc`.
  - Evidence (2.1.296 strings): the flag parser is `if(e==="")return[]` and otherwise splits the
    value on `,`, rejecting anything but `user`, `project` and `local`. The CLI builds the same
    `--setting-sources=<v>` form when it relaunches itself, and the Python SDK sends
    `--setting-sources=` with an empty list.
  - Live: the login still works (auth is not a setting), and the user's SessionStart hook stopped
    running.
  - Not loaded either: project skills, agents and commands under `.claude/`, and the automatic
    approval of the repo's `.mcp.json` servers (both gated on the project source). A host that wants
    agents and skills can load them through a plugin (below). Whether the
    project's `CLAUDE.md` is still read was not established: with a `CLAUDE.md` in the work dir,
    the init line's `memory_paths` listed only the auto-memory directory.
  - `Options.SettingSources` opts in. A host should run each login from a dedicated
    `CLAUDE_CONFIG_DIR`, and add `user` only deliberately, after checking what that config's
    settings allow.
- **Agents and skills without setting sources: `--plugin-dir`.** Probed on 2.1.296 (2026-10-23)
  with an empty `CLAUDE_CONFIG_DIR` (no login needed: the `initialize` reply lists `agents` and the
  skills among `commands`, before any model call), `HOME` set to a scratch directory, and
  `--setting-sources=` empty:
  - Nothing under `<config dir>/agents`, `<config dir>/skills`, `$HOME/.claude/agents|skills` or the
    working directory's `.claude/agents|skills` loads (all gated on a setting source). The working
    directory's `.claude/settings.json` SessionStart hook did not run.
  - `--add-dir=<dir>` loads no agents or skills either.
  - `--agents=<json>` (or a file path with `-p`) adds agents under their plain name. Agents only, and
    the JSON form takes the SDK's fields (including `permissionMode`, `hooks`, `mcpServers`).
  - **`--plugin-dir=<dir>` loads both**: `<dir>/agents/*.md` and `<dir>/skills/<name>/SKILL.md` show
    as `<plugin>:<name>` (the name in `<dir>/.claude-plugin/plugin.json`, else the folder's name).
    Repeatable. For plugin agents the CLI ignores `permissionMode`, `hooks` and `mcpServers` (and
    `initialPrompt`, `observer*`) with a warning (strings: `m3t=["permissionMode","hooks","mcpServers"]`),
    and takes `name`, `description`, `tools`, `skills`, `model`, `effort`, `color`, `memory`,
    `isolation`, `background`; an agent file over 1 MiB is skipped.
  - A host that needs agents and skills with no setting sources can write a plugin directory of
    its own (a manifest, `agents/`, `skills/`) and pass it as `Options.PluginDirs`
    (`--plugin-dir=`). Setting sources stay empty, so the repo's settings and hooks still never
    load. A plugin can also bring hooks, MCP servers and settings, so write only what you mean to.
  - **Plugin hooks run.** A plugin's `hooks/hooks.json` SessionStart hook ran at start-up, before
    any model call (probed 2026-10-23: the hook's command created a file). With
    `--settings={"disableAllHooks": true}` it did not run. A host that uses no CLI hooks should
    pass `disableAllHooks` (through `ExtraArgs`), write the plugin where the agent cannot write it,
    and write it again before every start. In-process hooks (`Extra.Hooks`) are separate: they are
    registered through the control protocol.
  - `--settings={"disableSkillShellExecution": true}` is accepted with no setting sources (no
    warning on stderr). The setting's description (2.1.296 strings): "Disable inline shell
    execution in skills and custom slash commands from user, project, or plugin sources. Commands
    are replaced with a placeholder instead of being run." Pass it (through `ExtraArgs`) when a run
    loads skills you do not want to run shell commands. That the placeholder replaces a command at skill load was read from the code
    (`disableSkillShellExecution===!0` → the `` !`...` `` patterns are replaced), not exercised:
    invoking a skill needs a model turn.
  - Not verified here (needs a login and a model turn): that the model sees the plugin's agents in the
    Task tool and the skills in its skill list, and the init line's `agents`/`skills`/`plugins` keys.
    `TestRealCLI` would show them on a machine with a login.
- Exit: after stdin EOF the CLI finishes the run and exits 0. After an error result
  (`error_max_turns`, an API error) it exits non-zero on purpose, so the driver puts the result's
  error text into `agent.ExitError.Result`. stderr is kept as a 40-line tail.

## Lines

Each line on stdout is one JSON object. The driver keeps **every line raw** (`Event.Raw`, exact
bytes without the newline). It also emits every line it writes to stdin as `KindSent`, and every
stderr line as `KindStderr`, so the host keeps the complete transcript. Typed parsing is a
convenience on top: unknown types become `KindUnknown`, non-JSON lines become `KindUnknown` with
`ParseError`, and nothing is dropped. Lines over 64 MiB are delivered truncated, with a
`ParseError`.

| `type` (`subtype`) | Kind | Notes |
|---|---|---|
| `system` (`init`) | `KindInit` | cwd, session_id, model, permissionMode, tools, mcp_servers (with status), claude_code_version, plus agents, skills, slash_commands, plugins, memory_paths, messaging_socket_path, ... Sent again at the start of **every** turn, not once. |
| `system` (`session_state_changed`) | `KindSessionState` | `state`. One Write-prompt run went running, requires_action (prompt open), running, idle. |
| `system` (other) | `KindSystem` | seen: hook_started, hook_response, status, thinking_tokens, commands_changed, task_summary, post_turn_summary, task_started, task_updated, task_notification, background_tasks_changed |
| `assistant` | `KindAssistant` | One content block per line; thinking and tool_use arrive as separate lines with the same message id. Extra fields seen: `wire_tool_inputs`, `tool_use_meta`. |
| `user` | `KindUser` | Tool results (`tool_use_result`, `tool_result_meta` carry details), and `[Request interrupted by user]`. `content` may be a plain string. |
| `result` | `KindResult` | Ends a **turn**, not the session. subtype, is_error, result, errors[], num_turns, duration_ms, total_cost_usd, stop_reason, terminal_reason, deferred_tool_use. `type` is not the first key. |
| `rate_limit_event` | `KindRateLimit` | see below |
| `stream_event` | `KindStreamDelta` | only with `--include-partial-messages` |
| `control_request` / `control_response` / `control_cancel_request` | `KindControl` | see below |

### Rate limits

```json
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1792882800,
 "rateLimitType":"seven_day","utilization":0.8,"overageStatus":"rejected",
 "overageDisabledReason":"org_level_disabled","isUsingOverage":false,"surpassedThreshold":0.75,
 "unifiedWindows":{"five_hour":{"utilization":0.31,"resetsAt":1792584000},
 "seven_day":{"utilization":0.8,"resetsAt":1792882800}},"overageEnabled":false},
 "uuid":"…","session_id":"…"}
```

The fields are nested under `rate_limit_info`, and `resetsAt` is in Unix seconds. Parsed into
`agent.RateLimit{Status, Type, ResetsAt, Utilization, OverageStatus}`; the raw line keeps the
rest. On 2.1.296 it was emitted after every model response with an unchanged status, not only when
the status changed. Treat it as a snapshot. `rejected` means the run should pause until
`ResetsAt`. Under `-p` nothing waits out a limit by itself.

## Control protocol

Both sides send requests and correlate replies by `request_id`:

```json
{"type":"control_request","request_id":"req_1_9f3a…","request":{"subtype":"…", …}}
{"type":"control_response","response":{"subtype":"success","request_id":"…","response":{…}}}
{"type":"control_response","response":{"subtype":"error","request_id":"…","error":"…"}}
{"type":"control_cancel_request","request_id":"…"}
```

Our ids are `req_<n>_<hex>`; the CLI's are UUIDs. Requests from the CLI are served each on their
own goroutine, so a person taking an hour to answer a prompt blocks nothing else.

- **Unbounded event queue (a deliberate trade-off).** The event stream in front of the consumer is
  an unbounded queue, so control traffic is never held up by a slow consumer, and nothing is
  dropped or deadlocked. The cost is memory: the consumer must keep up with `Events()`.
- **Requests the driver cannot read.** A request whose id cannot be read gets no reply, because
  there is nothing to address one to. A request with an id but an unreadable body (no subtype,
  wrong field types) gets an error reply, which the CLI treats as a denial. A control line over
  64 MiB is delivered truncated as `KindUnknown` and never served.
- **Callbacks are not waited for at the end.** When the CLI exits, every running callback's
  context is cancelled, and `Wait`, `Close` and the `Events` channel finish without waiting.
  A callback that ignores its context (a goroutine still waiting for a phone answer) cannot hold
  them up, and its late reply is dropped because input is closed. The CLI may reuse a request id;
  each handler removes only its own entry, so a later cancel still reaches the newer request.
- **Writes.** A written line is queued as `KindSent` *before* it is written, so the transcript
  never shows a reply before its cause. A write that fails adds a `KindDriver` event
  (`write_failed`, with `Err`). `Send`, `Interrupt` and the other calls apply their context to a
  write blocked on a full pipe, as a write deadline. A write cut off part way leaves a broken line
  in the CLI's input, so the driver then closes input, and the session can only finish.

### Driver → CLI

| subtype | payload | reply |
|---|---|---|
| `initialize` | `hooks` (null, or event → `[{matcher, hookCallbackIds, timeout}]`) | commands, agents, models, account, pid, `claude_code_version`, `current_permission_mode`, `session_state`, `capabilities` (`["ui_surface_v1"]`), output styles, … |
| `interrupt` | — | `{"still_queued":[]}`, then a `user` "[Request interrupted by user]" line and a `result` |
| `set_permission_mode` | `mode` | `Session.SetPermissionMode` |
| `set_model` | `model` | `Session.SetModel` |
| anything else | | `Session.Request` (e.g. `mcp_status`, `get_context_usage`, `stop_task`) |

`Start` returns only after `initialize` succeeds (60s timeout by default). The **SDK MCP servers
are initialized while `initialize` is still pending**: the CLI sends their `mcp_message`
initialize first and answers ours afterwards. A driver that blocked on its own reply before
serving requests would deadlock here.

### CLI → driver

**`can_use_tool`** (permission prompt):

```json
{"subtype":"can_use_tool","tool_name":"Write","display_name":"Write","input":{…},
 "description":"hello.txt","tool_use_id":"toolu_…",
 "permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}],
 "decision_reason":"…","blocked_path":"…","agent_id":"…","title":"…"}
```

- The reply is `{"behavior":"allow","updatedInput":{…}}`, with the original input when unchanged,
  as the SDKs send it. `"updatedPermissions":[…]` is optional.
- A deny is `{"behavior":"deny","message":"…","interrupt":true?}`.
- A callback error or panic becomes an error reply, which the CLI treats as a denial.
- In-process (SDK MCP) tools are permission-checked too: their request carries
  `mcp_server:{name, source:"sdk"}`. List them in `AllowedTools` (`mcp__<server>__<tool>`) to skip
  the prompt.

**`AskUserQuestion`** arrives as a `can_use_tool` with `tool_name:"AskUserQuestion"` and
`requires_user_interaction:true`:

```json
"input":{"questions":[{"question":"Which color do you prefer?","header":"Color","multiSelect":false,
  "options":[{"label":"Red","description":"…"},{"label":"Blue","description":"…"}]}]}
```

- The answer is an **allow whose updatedInput is the original input plus `answers`**, mapping each
  question text to the chosen label, or to free text:
  `{"behavior":"allow","updatedInput":{"questions":[…],"answers":{"Which color do you prefer?":"Blue"}}}`.
- Several labels for a multi-select question are sent as an array, so a label containing ", "
  stays intact. On 2.1.296 the tool's input schema accepts a string or an array of strings per
  answer: `answers: record(string, preprocess(array → join(", "), string))`. So the model sees the
  same `"A, B"` text either way.
- An allow of a question **without answers** is refused with an error reply (a denial), so a
  blanket auto-allow cannot silently skip a question meant for a person.
- `Question.Raw` keeps each question as sent. Newer, feature-gated question kinds carry fields
  the struct does not model (`kind`, `min`, `max`, `step`, `unit`, `placeholder`).
- **No question timeout applies to the host.** On 2.1.296 the `askUserQuestionTimeout` setting
  (`60s`/`5m`/`10m`/`never`, default `never`) only feeds the terminal dialog component: its getter
  is read in the Ink `AskUserQuestion` renderer, which passes it on as `afkTimeoutMs`.
  - An answer from the SDK host goes through a validator that rejects `afkTimeoutMs` in
    `updatedInput` as a "host_field", so a host-answered question cannot time out into "the user
    may be away from keyboard. Proceed using your best judgment".
  - The related `dialogExpiry` setting (default 5 minutes, overridden by
    `CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS`) applies to `request_user_dialog` requests and to dialogs
    forwarded to remote clients. Its own text says "local-only permission prompts (no remote
    client) are unaffected".
  - No timeout was found on the `can_use_tool` path to the host. The driver declares no dialog
    kinds, so it is not sent `request_user_dialog`, and with no setting sources a user setting
    cannot turn either timeout on. The driver therefore passes no `--settings` for them.
- The model then sees the tool result
  `Your questions have been answered: "Which color do you prefer?"="Blue". …`.
- The driver parses `questions` into `PermissionRequest.Questions`, and `agent.Answer(map)` builds
  the reply.
- The CLI offers `AskUserQuestion` only when something can answer (`--permission-prompt-tool
  stdio`), and never inside subagents.

**`mcp_message`**: in-process tools. See below.

**`hook_callback`**: `{callback_id, input, tool_use_id}`. The reply is the hook output object.

**Hook failures fail closed for PreToolUse.** The CLI treats a hook callback that errors as "no
opinion" and carries on: on 2.1.296 it logs `Error in hook callback …` and uses `{}`. For a
PreToolUse hook that gates tools, that would mean the tool runs. So the driver turns a PreToolUse
`HookFunc` error, a panic, or an output that is not a JSON object into
`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"hook failed: …"}}`.
For every other event, a failure is sent as an error reply, and the CLI proceeds as if the hook
had returned nothing.

`claudecode.DeferToolUse(reason)` builds the PreToolUse `permissionDecision:"defer"` output: the
CLI stops, the `result` carries `deferred_tool_use`, and the session is resumed with `--resume`
later. A host does not usually need this, because a `can_use_tool` callback can simply
wait. Defer is for answers slow enough that the process should not be kept alive.

**`control_cancel_request`**: the CLI withdrew a request. The callback's context is cancelled and
no reply is written.

### In-process tools (`mcp_message`)

1. The host passes `agent.Options.ToolServers`. Each server is declared on the command line as
   `--mcp-config={"mcpServers":{"tools":{"type":"sdk","name":"tools"}}}`; there is no process or
   socket.
2. The CLI sends each JSON-RPC message for that server as
   `{"subtype":"mcp_message","server_name":"tools","message":{…jsonrpc…}}`, and the reply is
   `{"mcp_response":{…jsonrpc…}}`.
3. The order seen on 2.1.296 is `initialize` (protocolVersion `2025-11-25`, during our
   initialize), then `notifications/initialized`, then `tools/list`, then `tools/call` per use.
   `tools/call` params carry `_meta["claudecode/toolUseId"]`.
4. The driver answers `initialize`, `ping`, `tools/list` and `tools/call`. Notifications get the
   ack `{"jsonrpc":"2.0","result":{}}`, unknown methods get -32601, unknown tools get -32602, and
   an unknown server gets -32601 "Server 'x' not found". A handler error or panic becomes a result
   with `isError:true`, which the model reads.
5. The model sees the tools as `mcp__<server>__<tool>`. On 2.1.296 MCP tools are **deferred**: the
   model first calls `ToolSearch` (`select:mcp__tools__echo`) to load the schema. That costs one
   extra turn, so budget `MaxTurns` for it.

### User messages (follow-ups)

```json
{"type":"user","session_id":"","message":{"role":"user","content":"…" | [blocks]},
 "parent_tool_use_id":null,"priority":"now"|"next"|"later"}
```

This is a real user turn, the same as typing in the chat. It can be sent at any time. Verified:
after an interrupt, a `priority:"now"` message started a new turn at once.

## Gotchas

- **Always pass `--permission-mode`.** Without it the CLI takes `defaultMode` from the user's
  settings, and on the test machine initialize reported `bypassPermissions`. `--help` lists `acceptEdits,
  auto, bypassPermissions, manual, dontAsk, plan`, but `default` is still accepted (initialize
  reports `current_permission_mode:"default"`). Resuming does not restore the mode, so pass it on
  every start.
- **The user's environment applies under `-p` unless excluded.** With the user setting source,
  user settings, hooks (in the test, a SessionStart hook and a PreToolUse hook) and
  plugins load. claude.ai MCP connectors load unless `StrictMCPConfig` is set. The driver loads no
  setting sources by default. Run each login from a dedicated `CLAUDE_CONFIG_DIR`, and use
  `StrictMCPConfig`.
- **A `result` ends a turn, not the session.** Background tasks finishing wake the agent for more
  turns: one session produced three results. Use `session_state_changed` = `idle` after a result
  to decide the agent has nothing left. Close stdin only then: once stdin is closed, permission
  prompts, hooks and in-process tools fail with "Stream closed".
- **Never run two processes on one session id.**
- The init line repeats per turn, so read the session id from it, but do not treat a second init
  as a new session.
- `--version` prints `2.1.296 (Claude Code)`. The initialize reply also carries
  `claude_code_version`; both are checked.

## Version policy: refuse by default

`Driver.Start` runs `<binary> --version`, symlinks resolved, so the version checked is the binary
that runs. The result is cached per file. The initialize reply's `claude_code_version` is checked
again. A version other than `TestedVersion` fails with `*claudecode.VersionError`, unless
`Driver.AllowUntestedVersion` is set; then it runs, with `ClaudeInfo().VersionWarning` set.

Why refuse rather than warn: a protocol drift is silent. A renamed field means a question that is
never answered, a lost transcript line, or a permission granted on the wrong input, and a warning
in a log is not seen until a run goes wrong. A refusal is loud, and the host can report
"agent CLI version X is not supported yet" to the person who started the run. The escape hatch exists for the upgrade
check below. To keep the pin, start from the versioned binary
(`~/.local/share/claude/versions/<v>`) rather than the `~/.local/bin/claude` symlink, and keep
`DISABLE_AUTOUPDATER=1`, which the driver sets.

## Re-verifying on a CLI upgrade

1. Install the new version alongside the old one (`claude install <version>`, which lands in
   `~/.local/share/claude/versions/<version>`).
2. Diff `claude --help` between the versions for the flags above, the permission-mode choices and
   `--permission-prompts`. Check the hidden flags still parse:
   `<bin> -p --permission-prompt-tool=stdio --max-turns=1 --input-format=stream-json
   --output-format=stream-json --verbose </dev/null` must not print "unknown option".
3. Read the Python SDK's changes since the commit above in `_internal/query.py`,
   `transport/subprocess_cli.py`, `types.py` and `message_parser.py`, and look for new control
   subtypes, renamed fields and new required flags.
4. Run the fake-CLI suite (`go test -race ./...`), then the real one against the new binary:
   ```
   CCGO_CLAUDE_IT=1 CCGO_CLAUDE_ALLOW_UNTESTED=1 \
   CCGO_CLAUDE_BIN=~/.local/share/claude/versions/<version> \
     go test -count=1 -run TestRealCLI -v .
   ```
   It must pass with no parse errors. Read its log for new system subtypes and init keys.
5. Bump `TestedVersion` and the version at the top of this page, and note what changed.
