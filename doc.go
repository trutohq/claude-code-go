// Package claudecode drives the Claude Code CLI (`claude`) as a subprocess
// over the stream-json control protocol that Anthropic's Agent SDKs speak.
//
// This is not an official Anthropic SDK. It is an independent Go driver for
// the same protocol, written to run unattended coding agents from a Go
// program.
//
// A Driver starts a Session. The session streams every line the CLI writes
// (and every line the driver writes to it) as agent.Event values, keeps the
// raw bytes of each one, and serves the CLI's control requests: permission
// prompts and AskUserQuestion go to agent.Options.Permission, hook callbacks
// to Extra.Hooks, and in-process MCP tools to agent.Options.ToolServers.
//
// The protocol is not a documented stable API, so the driver is pinned to one
// CLI version (TestedVersion) and refuses others unless
// Driver.AllowUntestedVersion is set. docs/protocol.md in the repository
// describes the protocol as implemented and how to re-verify it when the CLI
// is upgraded.
//
// Package agent holds the agent-neutral types (Options, Event, Session,
// PermissionFunc, ToolServer) that this package implements.
//
// Defaults are chosen for running an agent on code you do not fully trust:
// no settings files are loaded (no user, project or local settings, so a
// committed .claude/settings.json cannot allow tools or add hooks), the
// environment is an allowlist, the permission mode is always passed, and
// everything the CLI started is killed when it exits.
package claudecode
