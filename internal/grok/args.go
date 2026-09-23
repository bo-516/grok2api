package grok

import (
	"encoding/json"
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
	// StopAfterInit kills the process once system/init shows tools:[].
	// Startup uses this so the toolset check does not wait for a full completion.
	StopAfterInit bool
}

// ProbePrompt is the prompt-file body of the startup toolset probe.
// The fake grok recognizes it and prints only an init line. Real grok is killed
// at that line, so the text is never a user request.
const ProbePrompt = "agent-mock-toolset-probe\n"

// CommandArgs returns argv for a restricted grok run, not including argv0.
// promptFile and cwd must be absolute paths the caller created.
// The set forces an empty tool list: --tools todo_write is then removed by
// --disallowed-tools, which is the combination grok 1.0.41 actually honors
// (--tools "" leaves the default tools in place). permission-mode is only dontAsk.
// A wrong cwd would let grok read the developer's repo, so cwd is required.
func CommandArgs(promptFile, cwd string, spec RunSpec) []string {
	args := []string{
		"--prompt-file", promptFile,
		"--verbatim",
		"--output-format", "streaming-messages-json",
		"--include-partial-messages",
		"--max-turns", "1",
		"--no-subagents",
		"--no-plan",
		"--disable-web-search",
		"--tools", "todo_write",
		"--disallowed-tools", "search_tool,use_tool,todo_write",
		"--permission-mode", "dontAsk",
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
