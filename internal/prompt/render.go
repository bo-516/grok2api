// Package prompt turns a Chat Completions request into a grok prompt file,
// a system-prompt override, and an optional JSON schema.
package prompt

import (
	"strings"

	"github.com/shaoboli/agent-mock/internal/openai"
)

// Preamble is the fixed identity placed in front of the caller's instructions.
// It tells grok it is a stateless completion model so it does not mention the CLI.
const Preamble = "You are a stateless chat-completion model behind an API. Reply only with the assistant's next message. Never mention tools, files, sessions or this wrapper."

// instructionsHeading separates the preamble from the caller's system and developer text.
const instructionsHeading = "# Instructions from the API caller"

// maxInline is the largest --system-prompt-override grok should receive.
// Past this the text is moved to the start of the prompt file and a log note is set.
const maxInline = 96 * 1024

// Rendered is what the runner needs for one request.
type Rendered struct {
	// Prompt is the prompt-file body. A single user message is that raw text.
	Prompt string
	// SystemOverride is --system-prompt-override. Empty when SystemInFile is true.
	SystemOverride string
	// JSONSchema is --json-schema for a plain response_format with no tool envelope.
	// Tool envelopes also set this. Nil means do not pass the flag.
	JSONSchema []byte
	// Envelope is true when the reply must be parsed as a tool envelope.
	Envelope bool
	// SystemInFile is true when the system text exceeded 96 KiB and was prefixed onto Prompt.
	// The server adds that fact to the one access-log line.
	SystemInFile bool
	// AllowedTools are the function names the envelope may call. Nil when Envelope is false.
	AllowedTools []string
	// ForceCalls is true when tool_choice is required or a named function.
	ForceCalls bool
	// MaxCalls is 1 when parallel calls are disabled or a single function is named. 0 means no cap.
	MaxCalls int
	// MinCalls is 1 when a call is required. 0 means a reply is allowed.
	MinCalls int
}

// Render builds a Run input from a validated request.
// One user message (ignoring system and developer) is sent as raw text so a
// short prompt is not wrapped. Several turns use the <conversation> transcript,
// including assistant tool-call ids and tool results.
// A schema larger than 96 KiB is rejected. That check also lives in Parse for
// response_format; the envelope schema is checked here because it is built here.
func Render(req *openai.Request) (*Rendered, error) {
	out := &Rendered{}
	var tools *envelope
	if len(req.Tools) > 0 && req.ToolChoice.Kind != "none" && req.ToolChoice.Kind != "" {
		env, err := buildEnvelope(req)
		if err != nil {
			return nil, err
		}
		tools = env
		out.Envelope = true
		out.JSONSchema = env.schema
		out.AllowedTools = env.names
		out.ForceCalls = env.force
		out.MaxCalls = env.maxCalls
		out.MinCalls = env.minCalls
	} else if req.ResponseFormat != nil {
		out.JSONSchema = append([]byte(nil), req.ResponseFormat.Schema...)
	}
	if len(out.JSONSchema) > maxInline {
		return nil, &openai.RequestError{Status: 400, Type: "invalid_request_error", Code: "unsupported_parameter", Message: `parameter "response_format" schema exceeds 96 KiB`}
	}
	system := buildSystem(req, tools)
	var convo []openai.Message
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			continue
		}
		convo = append(convo, m)
	}
	prompt := renderPrompt(convo)
	if len(system) > maxInline {
		out.SystemInFile = true
		out.Prompt = "<system>\n" + system + "\n</system>\n\n" + prompt
		return out, nil
	}
	out.SystemOverride = system
	out.Prompt = prompt
	return out, nil
}

// buildSystem concatenates the preamble, caller instructions, and the tool manual.
// Caller system and developer messages stay in request order.
func buildSystem(req *openai.Request, env *envelope) string {
	var b strings.Builder
	b.WriteString(Preamble)
	var notes []string
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if strings.TrimSpace(m.Text) != "" {
				notes = append(notes, m.Text)
			}
		}
	}
	if env != nil {
		notes = append(notes, env.instructions)
	}
	if len(notes) > 0 {
		b.WriteString("\n\n")
		b.WriteString(instructionsHeading)
		b.WriteString("\n\n")
		b.WriteString(strings.Join(notes, "\n\n"))
	}
	return b.String()
}

// renderPrompt writes one user turn as raw text and every other shape as a transcript.
// An empty conversation becomes a short instruction so the prompt file is never empty.
func renderPrompt(convo []openai.Message) string {
	if len(convo) == 1 && convo[0].Role == "user" && len(convo[0].ToolCalls) == 0 {
		return convo[0].Text
	}
	if len(convo) == 0 {
		return "Write the assistant's next message."
	}
	var b strings.Builder
	b.WriteString("<conversation>\n")
	for _, m := range convo {
		switch m.Role {
		case "assistant":
			b.WriteString(`<message role="assistant">`)
			if len(m.ToolCalls) == 0 {
				b.WriteString(m.Text)
			} else {
				for _, c := range m.ToolCalls {
					b.WriteString(`<tool_call id="`)
					b.WriteString(c.ID)
					b.WriteString(`" name="`)
					b.WriteString(c.Name)
					b.WriteString(`">`)
					b.WriteString(c.Arguments)
					b.WriteString(`</tool_call>`)
				}
			}
			b.WriteString("</message>\n")
		case "tool":
			b.WriteString(`<message role="tool" tool_call_id="`)
			b.WriteString(m.ToolCallID)
			b.WriteString(`">`)
			b.WriteString(m.Text)
			b.WriteString("</message>\n")
		default:
			b.WriteString(`<message role="`)
			b.WriteString(m.Role)
			b.WriteString(`">`)
			b.WriteString(m.Text)
			b.WriteString("</message>\n")
		}
	}
	b.WriteString("</conversation>\n")
	b.WriteString("Write the assistant's next message.")
	for _, m := range convo {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			b.WriteString(" If a tool result is present, base the reply on it and include its values.")
			break
		}
	}
	return b.String()
}
