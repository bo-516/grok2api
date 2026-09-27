package grok

import (
	"encoding/json"
	"strconv"
	"strings"
)

// RunSpec is one grok invocation. Prompt is the body written to the 0600 prompt file,
// never an argv element. SystemOverride is --system-prompt-override, empty when the
// caller text was too large and already lives at the top of Prompt.
// Model, ReasoningEffort, and JSONSchema are omitted from argv when empty.
type RunSpec struct {
	// Prompt is the full prompt-file body.
	Prompt string
	// SystemOverride is passed as --system-prompt-override when non-empty.
	SystemOverride string
	// Model is the grok model for -m. Empty omits -m.
	Model string
	// ReasoningEffort is --reasoning-effort. Empty omits the flag.
	ReasoningEffort string
	// JSONSchema is --json-schema. Nil or empty omits the flag.
	// The streaming output format stays streaming-messages-json; grok 1.0.41 still
	// emits NDJSON when that format is set explicitly next to --json-schema.
	JSONSchema json.RawMessage
	// StopAfterInit kills the process once system/init has been checked.
	// Startup probes use this so the toolset check does not wait for a completion.
	// A probe rejects tools outside Tools and reports missing allowlist entries
	// to the caller instead of failing the run.
	StopAfterInit bool
	// Tools is the exact tool allowlist. Nil or empty means a chat run: argv still
	// passes --tools todo_write and disallows it, which is what yields tools:[].
	// A non-empty list is passed as --tools and the init record must match it.
	Tools []string
	// MaxTurns is --max-turns. Zero means 1, the chat value. A media plan sets 2
	// (the tool round plus the closing reply) or 3 for text-to-video.
	MaxTurns int
	// Cwd replaces the shared empty chat directory when set. Media runs pass mcwd.
	// Empty keeps <Root>/cwd. The runner creates the directory at 0700.
	Cwd string
	// ExtraEnv is appended after ChildEnv. Media sets the parallel image or video
	// call caps. XAI_API_KEY in this list is dropped so it cannot undo ChildEnv.
	ExtraEnv []string
	// PermissionMode is --permission-mode when Tools is non-empty.
	// Empty still uses dontAsk. Chat ignores this field and always passes dontAsk.
	// Media plans set bypassPermissions because dontAsk cancels image_gen on grok 1.0.41.
	PermissionMode string
	// SessionID is passed as --session-id, a UUID grok uses for the new session
	// instead of generating one. Runner.Run fills it with NewSessionID when empty,
	// so agent-mock can delete the session even if grok dies before printing its id.
	// A value that is not a fresh UUID makes grok exit with an error.
	SessionID string
}

// ProbePrompt is the prompt-file body of the startup toolset probe.
// The fake grok recognizes it and prints only an init line. Real grok is killed
// at that line, so the text is never a user request.
const ProbePrompt = "agent-mock-toolset-probe\n"

// CommandArgs returns argv for one grok run, not including argv0.
// promptFile and cwd must be absolute paths the caller created.
// When spec.Tools is empty the set forces an empty tool list: --tools todo_write
// is then removed by --disallowed-tools, which is the combination grok 1.0.41
// actually honors (--tools "" leaves the default tools in place). Chat
// permission-mode stays dontAsk and --max-turns stays 1.
// When spec.Tools is set, --tools is that allowlist and --disallowed-tools is
// only search_tool,use_tool so the media tools survive. A wrong cwd would let
// grok read the developer's repo, so cwd is required.
// spec.SessionID adds --session-id; empty leaves the id to grok.
func CommandArgs(promptFile, cwd string, spec RunSpec) []string {
	turns := spec.MaxTurns
	if turns <= 0 {
		turns = 1
	}
	tools := "todo_write"
	disallowed := "search_tool,use_tool,todo_write"
	perm := "dontAsk"
	if len(spec.Tools) > 0 {
		tools = strings.Join(spec.Tools, ",")
		disallowed = "search_tool,use_tool"
		if spec.PermissionMode != "" {
			perm = spec.PermissionMode
		}
	}
	args := []string{
		"--prompt-file", promptFile,
		"--verbatim",
		"--output-format", "streaming-messages-json",
		"--include-partial-messages",
		"--max-turns", strconv.Itoa(turns),
		"--no-subagents",
		"--no-plan",
		"--disable-web-search",
		"--tools", tools,
		"--disallowed-tools", disallowed,
		"--permission-mode", perm,
		"--cwd", cwd,
	}
	if spec.SystemOverride != "" {
		args = append(args, "--system-prompt-override", spec.SystemOverride)
	}
	if spec.Model != "" {
		args = append(args, "-m", spec.Model)
	}
	if spec.ReasoningEffort != "" {
		args = append(args, "--reasoning-effort", spec.ReasoningEffort)
	}
	if len(spec.JSONSchema) > 0 {
		args = append(args, "--json-schema", string(spec.JSONSchema))
	}
	if spec.SessionID != "" {
		args = append(args, "--session-id", spec.SessionID)
	}
	return args
}

// ChildEnv copies parent and removes XAI_API_KEY so a key in the developer shell
// cannot turn a logged-out grok into a metered API call.
// It forces the auto-updater off, memory off, and subagents off.
// parent entries for those three keys are dropped so the forced values win.
func ChildEnv(parent []string) []string {
	out := make([]string, 0, len(parent)+3)
	for _, kv := range parent {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "XAI_API_KEY", "GROK_DISABLE_AUTOUPDATER", "GROK_MEMORY", "GROK_SUBAGENTS":
			continue
		default:
			out = append(out, kv)
		}
	}
	out = append(out,
		"GROK_DISABLE_AUTOUPDATER=1",
		"GROK_MEMORY=0",
		"GROK_SUBAGENTS=0",
	)
	return out
}
