package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trutohq/claude-code-go/agent"
)

// TestedVersion is the Claude Code CLI version this driver was verified
// against. The protocol is not a documented stable API: upgrade by re-running
// the integration test (docs/protocol.md) and then bumping this.
const TestedVersion = "2.1.296"

// Name is the driver name.
const Name = "claude-code"

const (
	defaultInitTimeout   = 60 * time.Second
	defaultShutdownGrace = 5 * time.Second
	versionProbeTimeout  = 30 * time.Second
)

// Driver starts Claude Code sessions.
type Driver struct {
	// AllowUntestedVersion lets sessions start on a CLI version other than
	// TestedVersion. The mismatch is then reported in Info().VersionWarning
	// instead of failing Start.
	AllowUntestedVersion bool

	mu       sync.Mutex
	versions map[string]string // resolved binary + stat -> version
}

var _ agent.Driver = (*Driver)(nil)

// New returns a Driver that refuses untested CLI versions.
func New() *Driver { return &Driver{} }

// Name implements agent.Driver.
func (d *Driver) Name() string { return Name }

// Extra holds Claude-specific session options that have no place in the
// agent-neutral agent.Options.
type Extra struct {
	// Hooks registers in-process hook callbacks by hook event name
	// ("PreToolUse", "PostToolUse", "Stop", ...).
	Hooks map[string][]HookMatcher
	// InitTimeout bounds the initialize handshake (default 60s; MCP servers
	// start during it).
	InitTimeout time.Duration
	// ShutdownGrace is how long Close waits after closing stdin before
	// SIGTERM, and again before SIGKILL (default 5s).
	ShutdownGrace time.Duration
}

// HookMatcher is one hook registration.
type HookMatcher struct {
	// Matcher is the tool-name pattern for tool hooks ("" matches all).
	Matcher   string
	Timeout   time.Duration
	Callbacks []HookFunc
}

// HookFunc answers a hook callback. The returned JSON is the hook output in
// the CLI's format (e.g. {"hookSpecificOutput": {...}}); nil means "{}".
//
// The CLI treats a failed hook callback as "no opinion" and carries on. So
// for PreToolUse the driver turns an error, a panic or an output that is not
// a JSON object into an explicit permissionDecision "deny" with the error as
// the reason: a hook that gates tools fails closed. For every other event a
// failure is reported to the CLI as an error, and the CLI proceeds as if the
// hook had returned nothing.
type HookFunc func(ctx context.Context, in HookInput) (json.RawMessage, error)

// HookInput is one hook invocation.
type HookInput struct {
	CallbackID string
	ToolUseID  string
	// Input is the hook payload (hook_event_name, tool_name, tool_input, ...).
	Input json.RawMessage
}

// DeferToolUse is a PreToolUse hook output that makes the CLI stop before the
// tool runs and exit; the result carries the deferred call, and the session
// is resumed with --resume once the answer is known.
func DeferToolUse(reason string) json.RawMessage {
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "defer",
		"permissionDecisionReason": reason,
	}}
	b, _ := json.Marshal(out)
	return b
}

// Start implements agent.Driver.
func (d *Driver) Start(ctx context.Context, opts agent.Options) (agent.Session, error) {
	s, err := d.StartSession(ctx, opts, Extra{})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// VersionError reports a CLI version this driver has not been tested with.
type VersionError struct {
	Binary string
	Found  string
	Tested string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("claudecode: %s is Claude Code %s, but this driver is tested with %s; "+
		"re-run the protocol tests before upgrading (docs/protocol.md in github.com/trutohq/claude-code-go)", e.Binary, e.Found, e.Tested)
}

var versionRE = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

// ParseVersion extracts the version from `claude --version` output.
func ParseVersion(out string) (string, error) {
	m := versionRE.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("claudecode: no version in %q", strings.TrimSpace(out))
	}
	return m[1], nil
}

// ResolveBinary finds the claude executable and resolves symlinks, so that
// the version checked is the version run even if the symlink moves.
func ResolveBinary(path string) (string, error) {
	if path == "" {
		path = "claude"
	}
	p, err := exec.LookPath(path)
	if err != nil {
		return "", fmt.Errorf("claudecode: find claude binary: %w", err)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return p, nil
}

// Version runs `<binary> --version` and returns the parsed version. Results
// are cached per binary file (path, size and mtime).
func (d *Driver) Version(ctx context.Context, binary string, env []string) (string, error) {
	st, err := os.Stat(binary)
	if err != nil {
		return "", fmt.Errorf("claudecode: %w", err)
	}
	key := binary + "|" + strconv.FormatInt(st.Size(), 10) + "|" + st.ModTime().UTC().Format(time.RFC3339Nano)
	d.mu.Lock()
	v, ok := d.versions[key]
	d.mu.Unlock()
	if ok {
		return v, nil
	}
	ctx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claudecode: %s --version: %w", binary, err)
	}
	v, err = ParseVersion(string(out))
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	if d.versions == nil {
		d.versions = map[string]string{}
	}
	d.versions[key] = v
	d.mu.Unlock()
	return v, nil
}

func (d *Driver) checkVersion(binary, found string) (warning string, err error) {
	if found == TestedVersion {
		return "", nil
	}
	verr := &VersionError{Binary: binary, Found: found, Tested: TestedVersion}
	if d.AllowUntestedVersion {
		return verr.Error(), nil
	}
	return "", verr
}

var (
	uuidRE       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	serverNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func validate(opts agent.Options) error {
	if opts.SessionID != "" && opts.Resume != "" {
		return errors.New("claudecode: SessionID and Resume are mutually exclusive")
	}
	if opts.SessionID != "" && !uuidRE.MatchString(opts.SessionID) {
		return fmt.Errorf("claudecode: SessionID %q is not a UUID", opts.SessionID)
	}
	if opts.MaxTurns < 0 {
		return errors.New("claudecode: MaxTurns must not be negative")
	}
	if opts.AppendSystemPrompt != "" && opts.AppendSystemPromptFile != "" {
		return errors.New("claudecode: AppendSystemPrompt and AppendSystemPromptFile are mutually exclusive")
	}
	if opts.WorkDir == "" {
		return errors.New("claudecode: WorkDir is required")
	}
	if err := validateCredential(opts); err != nil {
		return err
	}
	for _, d := range opts.PluginDirs {
		if !filepath.IsAbs(d) {
			return fmt.Errorf("claudecode: plugin dir %q must be an absolute path", d)
		}
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			return fmt.Errorf("claudecode: plugin dir %s is not a directory", d)
		}
	}
	for _, src := range opts.SettingSources {
		switch src {
		case "user", "project", "local":
		default:
			return fmt.Errorf("claudecode: unknown setting source %q (user, project, local)", src)
		}
	}
	{
		st, err := os.Stat(opts.WorkDir)
		if err != nil {
			return fmt.Errorf("claudecode: work dir: %w", err)
		}
		if !st.IsDir() {
			return fmt.Errorf("claudecode: work dir %s is not a directory", opts.WorkDir)
		}
	}
	seen := map[string]bool{}
	for _, srv := range opts.ToolServers {
		if !serverNameRE.MatchString(srv.Name) {
			return fmt.Errorf("claudecode: tool server name %q must be letters, digits, '_' or '-'", srv.Name)
		}
		if seen[srv.Name] {
			return fmt.Errorf("claudecode: duplicate tool server %q", srv.Name)
		}
		seen[srv.Name] = true
		tools := map[string]bool{}
		for _, t := range srv.Tools {
			if t.Name == "" || t.Handler == nil {
				return fmt.Errorf("claudecode: tool server %q has a tool without a name or handler", srv.Name)
			}
			if tools[t.Name] {
				return fmt.Errorf("claudecode: tool server %q has duplicate tool %q", srv.Name, t.Name)
			}
			tools[t.Name] = true
			if len(t.InputSchema) > 0 && !json.Valid(t.InputSchema) {
				return fmt.Errorf("claudecode: tool %s/%s has an invalid input schema", srv.Name, t.Name)
			}
		}
	}
	return nil
}

// maxCredentialBytes bounds a credential: it is written into a pipe before
// the CLI starts, so it must fit the smallest pipe buffer (16 KiB on macOS).
const maxCredentialBytes = 4096

// credentialFDVar is the variable that names the pipe the CLI reads each
// credential kind from.
var credentialFDVar = map[string]string{
	agent.CredentialOAuthToken: "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
	agent.CredentialAPIKey:     "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
}

// credentialVars are the CLI's other ways to receive a credential. With a
// Credential set, none may come from Options.Env: the CLI would pick one of
// two credentials, and the variable would show the secret to the agent.
var credentialVars = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR"}

func validateCredential(opts agent.Options) error {
	c := opts.Credential
	if c == nil {
		return nil
	}
	if _, ok := credentialFDVar[c.Kind]; !ok {
		return fmt.Errorf("claudecode: credential kind %q cannot be passed through a file descriptor (use %s or %s)", c.Kind, agent.CredentialOAuthToken, agent.CredentialAPIKey)
	}
	if strings.TrimSpace(c.Secret) == "" {
		return errors.New("claudecode: the credential is empty")
	}
	if len(c.Secret) > maxCredentialBytes || strings.ContainsAny(c.Secret, "\x00\r\n") {
		return fmt.Errorf("claudecode: the credential must be one line of at most %d bytes", maxCredentialBytes)
	}
	for _, k := range credentialVars {
		if _, ok := opts.Env[k]; ok {
			return fmt.Errorf("claudecode: Options.Env sets %s while a Credential is given", k)
		}
	}
	return nil
}

// DefaultPermissionMode is passed when Options.PermissionMode is empty. The
// mode is always passed explicitly: without it the CLI uses the user's
// settings, which on a developer machine may be bypassPermissions.
const DefaultPermissionMode = "default"

// buildArgs builds the CLI arguments. Values are passed as --flag=value so a
// value that starts with "-" can never be read as another flag.
func buildArgs(opts agent.Options) ([]string, error) {
	args := []string{
		"-p",
		"--input-format=stream-json",
		"--output-format=stream-json",
		"--verbose", // stream-json output requires it
	}
	add := func(flag, v string) {
		if v != "" {
			args = append(args, "--"+flag+"="+v)
		}
	}
	add("session-id", opts.SessionID)
	add("resume", opts.Resume)
	add("model", opts.Model)
	add("effort", opts.Effort)
	add("append-system-prompt", opts.AppendSystemPrompt)
	add("append-system-prompt-file", opts.AppendSystemPromptFile)
	if len(opts.AllowedTools) > 0 {
		add("allowedTools", strings.Join(opts.AllowedTools, ","))
	}
	if len(opts.DisallowedTools) > 0 {
		add("disallowedTools", strings.Join(opts.DisallowedTools, ","))
	}
	mode := opts.PermissionMode
	if mode == "" {
		mode = DefaultPermissionMode
	}
	add("permission-mode", mode)
	// Always passed: an empty value loads no settings files at all, so a
	// committed .claude/settings.json cannot allow tools or add hooks.
	args = append(args, "--setting-sources="+strings.Join(opts.SettingSources, ","))
	if opts.MaxTurns > 0 {
		add("max-turns", strconv.Itoa(opts.MaxTurns))
	}
	if opts.Permission != nil {
		// Permission prompts (and AskUserQuestion) arrive as can_use_tool
		// control requests on stdout.
		add("permission-prompt-tool", "stdio")
	} else {
		// Nobody answers: the CLI denies anything that would prompt, and
		// does not offer AskUserQuestion.
		add("permission-prompts", "none")
	}
	for _, c := range opts.MCPConfig {
		add("mcp-config", c)
	}
	if len(opts.ToolServers) > 0 {
		servers := map[string]any{}
		for _, s := range opts.ToolServers {
			servers[s.Name] = map[string]string{"type": "sdk", "name": s.Name}
		}
		b, err := json.Marshal(map[string]any{"mcpServers": servers})
		if err != nil {
			return nil, err
		}
		add("mcp-config", string(b))
	}
	if opts.StrictMCPConfig {
		args = append(args, "--strict-mcp-config")
	}
	// Plugins are loaded with no setting sources: their agents and skills
	// show as "<plugin>:<name>" (verified on 2.1.296, docs/protocol.md).
	for _, d := range opts.PluginDirs {
		add("plugin-dir", d)
	}
	if opts.IncludePartialMessages {
		args = append(args, "--include-partial-messages")
	}
	args = append(args, opts.ExtraArgs...)
	return args, nil
}

// SessionMarkerEnv is set to a random id in every CLI the driver starts.
// Children inherit it, even ones that leave the process group (the CLI starts
// its Bash shells with setsid), so on Linux the driver can find and kill
// everything the session left running. See killMarked.
const SessionMarkerEnv = "CCGO_AGENT_SESSION"

// allowedEnv is what is inherited by default: enough to run the CLI and the
// tools it starts, none of the host program's credentials.
var allowedEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TERM", "TMPDIR",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
}

var allowedEnvPrefixes = []string{"LC_"}

// parentSessionEnv are variables a parent Claude Code session sets for its
// own children (the CLI's own child-session list on 2.1.296, minus SHELL and
// TMPDIR, which the allowlist keeps, plus a few more seen in practice).
// Inherited by a session we start, they would make it think it runs inside
// that parent (wrong entrypoint, messaging socket, session id, trace, git
// config, temp dirs). They are dropped from the inherited environment in
// both modes; Options.Env may still set them explicitly.
var parentSessionEnv = []string{
	"GIT_EDITOR", "CLAUDECODE", "AI_AGENT", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_PID", "TRACEPARENT", "TRACESTATE", "CLAUDE_CODE_EXECPATH",
	"TMUX", "CLAUDE_CODE_TMPDIR", "TMPPREFIX", "BUN_OPTIONS", "TEMP", "TMP", "GIT_CONFIG_PARAMETERS",
	"CLAUDE_EFFORT", "CLAUDE_CODE_INVOKED_SKILLS",
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_WORKER_EPOCH", "CLAUDE_CODE_ENVIRONMENT_KIND",
	SessionMarkerEnv,
}

var parentSessionEnvPrefixes = []string{
	"CLAUDE_CODE_MESSAGING_",
	"CLAUDE_CODE_BRIDGE_",
	"CLAUDE_REMOTE_CONTROL_",
}

func isParentSessionVar(name string) bool {
	for _, n := range parentSessionEnv {
		if name == n {
			return true
		}
	}
	for _, p := range parentSessionEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func isAllowedVar(name string) bool {
	for _, n := range allowedEnv {
		if name == n {
			return true
		}
	}
	for _, p := range allowedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// buildEnv builds the CLI's environment from base (normally os.Environ()):
// the allowlist (or everything, with InheritEnv) minus parent-session
// variables, then the driver's own settings, then Options.Env, unfiltered.
func buildEnv(base []string, opts agent.Options, marker string) []string {
	env := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := env[k]; !ok {
			order = append(order, k)
		}
		env[k] = v
	}
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || isParentSessionVar(k) {
			continue
		}
		if !opts.InheritEnv && !isAllowedVar(k) {
			continue
		}
		if opts.Credential != nil && slices.Contains(credentialVars, k) {
			continue // the Credential is the only one the CLI gets
		}
		set(k, v)
	}
	// "sdk-cli" is the CLI's own entrypoint name for a non-language SDK host.
	set("CLAUDE_CODE_ENTRYPOINT", "sdk-cli")
	// The version is pinned; never let the CLI replace itself mid-run.
	set("DISABLE_AUTOUPDATER", "1")
	// Report running / idle / requires_action, so the host can tell "a
	// turn ended" from "the agent has nothing left to do".
	set("CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS", "1")
	for k, v := range opts.Env {
		set(k, v)
	}
	if opts.ConfigDir != "" {
		set("CLAUDE_CONFIG_DIR", opts.ConfigDir)
	}
	if opts.WorkDir != "" {
		set("PWD", opts.WorkDir)
	}
	if marker != "" {
		set(SessionMarkerEnv, marker)
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+env[k])
	}
	return out
}

// SessionFile is where Claude Code keeps a session's conversation:
// <config dir>/projects/<cwd with every character other than a letter or a
// digit replaced by "-">/<session id>.jsonl. A session is resumed (--resume)
// from the project directory of the working directory it is resumed in, so
// carrying a conversation to another working directory or machine means
// copying this file to the new directory's path. cwd should be the real
// path (symlinks resolved), as the CLI sees it.
//
// Not verified against the CLI by a test here (the fake CLI follows the
// same layout); for working directories longer than 200 characters the CLI
// shortens the name with a hash this function does not reproduce, and a
// resume then starts afresh.
func SessionFile(configDir, cwd, sessionID string) string {
	b := []byte(cwd)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return filepath.Join(configDir, "projects", string(b), sessionID+".jsonl")
}
